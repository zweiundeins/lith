package events

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/zweiundeins/lith/writer"
)

// Projector keeps a read model current from the events of one kind of
// stream. Its handlers are inline (Handle), running in the transaction of
// the command that appends, or catch-up (After), running after the commit
// from a checkpoint; one projector has one or the other. Every event type
// of the stream is handled or ignored (Ignore), or New refuses the
// projector.
type Projector[K Key, E any] struct {
	core     *projector
	stream   *Stream[K, E]
	handlers map[string]Handler[K, E]
}

// projector is a Projector with its types erased, as the store keeps it.
type projector struct {
	name        string
	version     int
	kind        string
	topic       string
	stream      any
	tables      []string
	catchUp     bool
	skipHistory bool
	handled     map[string]bool // the names handled or ignored
	types       func() []string // the stream's event types
	errs        []error

	typed any // the *Projector[K, E]
	// replay applies stored events with the inline handlers, deliver one
	// with the catch-up handler.
	replay  func(st *Store, tx *writer.Tx, evs []Raw) error
	deliver func(ctx context.Context, st *Store, ev Raw) error
}

// Handler is what a projector does with one event type.
type Handler[K Key, E any] struct {
	name   string
	inline func(*writer.Tx, Record[K, E]) error
	after  func(context.Context, Record[K, E]) error
}

// Handle has an inline projector run fn for every event of type t, in the
// transaction of the command that appends it. fn writes the projector's
// tables and touches the topics of what it changed.
func Handle[K Key, E, T any](t Type[K, E, T], fn func(*writer.Tx, Record[K, T]) error) Handler[K, E] {
	return Handler[K, E]{name: t.name, inline: func(tx *writer.Tx, r Record[K, E]) error {
		return fn(tx, retype[K, E, T](r))
	}}
}

// After has a catch-up projector run fn for every event of type t, after
// the commit that stored it, outside the writer: for mail, a search
// service, another system's API. It may see an event more than once,
// after a failure or a restart; r.Position is the idempotency key to hand
// on. An error is retried with backoff; one that persists parks the
// projector (Runner).
func After[K Key, E, T any](t Type[K, E, T], fn func(ctx context.Context, r Record[K, T]) error) Handler[K, E] {
	return Handler[K, E]{name: t.name, after: func(ctx context.Context, r Record[K, E]) error {
		return fn(ctx, retype[K, E, T](r))
	}}
}

// Ignore says that a projector does not need events of type t, so that
// New can tell a forgotten type from one left out on purpose.
func Ignore[K Key, E, T any](t Type[K, E, T]) Handler[K, E] {
	return Handler[K, E]{name: t.name}
}

func retype[K Key, E, T any](r Record[K, E]) Record[K, T] {
	return Record[K, T]{Position: r.Position, ID: r.ID, Version: r.Version, Type: r.Type, Data: any(r.Data).(T), Meta: r.Meta}
}

// NewProjector makes a projector of stream s's events. Its name is its
// row in lith_projections, and version the version of its handlers: when
// an inline projector's version changes, Runner rebuilds its tables, and a
// catch-up projector starts again from the first event.
func NewProjector[K Key, E any](s *Stream[K, E], name string, version int, hs ...Handler[K, E]) *Projector[K, E] {
	p := &Projector[K, E]{stream: s, handlers: map[string]Handler[K, E]{}}
	c := &projector{name: name, version: version, kind: s.kind, topic: "lith:projection:" + name,
		stream: s, handled: map[string]bool{}, types: s.Types, typed: p}
	p.core = c
	if name == "" {
		c.errs = append(c.errs, errors.New("events: a projector needs a name"))
	}
	inline, after := 0, 0
	for _, h := range hs {
		if c.handled[h.name] {
			c.errs = append(c.errs, fmt.Errorf("events: projector %s names %s twice", name, h.name))
		}
		c.handled[h.name] = true
		p.handlers[h.name] = h
		switch {
		case h.inline != nil:
			inline++
		case h.after != nil:
			after++
		}
	}
	if inline > 0 && after > 0 {
		c.errs = append(c.errs, fmt.Errorf("events: projector %s has inline (Handle) and catch-up (After) handlers: make two projectors", name))
	}
	c.catchUp = after > 0
	c.replay = p.replay
	c.deliver = p.deliver
	return p
}

// Tables names the tables an inline projector writes, and only it: a
// rebuild fills a copy of each and puts the copies in their place. The
// app's migrations create and change them. Without tables, a version
// change rebuilds nothing.
func (p *Projector[K, E]) Tables(names ...string) *Projector[K, E] {
	p.core.tables = append(p.core.tables, names...)
	for _, n := range names {
		if n == "" {
			p.core.errs = append(p.core.errs, fmt.Errorf("events: projector %s names an empty table", p.core.name))
		}
	}
	return p
}

// SkipHistory starts a new catch-up projector, and one whose version
// changed, after the events stored by then, instead of at the first: for
// a mailer that must not send the mail of the past.
func (p *Projector[K, E]) SkipHistory() *Projector[K, E] {
	p.core.skipHistory = true
	return p
}

// Name returns the projector's name.
func (p *Projector[K, E]) Name() string { return p.core.name }

// Topic is touched when a rebuild puts the projector's new tables in
// place: a view that reads them lists it.
func (p *Projector[K, E]) Topic() string { return p.core.topic }

func (p *Projector[K, E]) add(st *Store) error {
	c := p.core
	errs := c.errs
	if _, dup := st.byName[c.name]; dup {
		errs = append(errs, fmt.Errorf("events: two projectors named %s", c.name))
	}
	if c.catchUp && len(c.tables) > 0 {
		errs = append(errs, fmt.Errorf("events: catch-up projector %s has tables: only inline projectors are rebuilt", c.name))
	}
	if !c.catchUp && c.skipHistory {
		errs = append(errs, fmt.Errorf("events: inline projector %s skips history: only catch-up projectors can", c.name))
	}
	errs = append(errs, st.stream(c.kind, p.stream))
	if err := errors.Join(errs...); err != nil {
		return err
	}
	st.projectors = append(st.projectors, c)
	st.byName[c.name] = c
	if c.catchUp {
		st.catchUp[c.kind] = append(st.catchUp[c.kind], c)
	} else {
		st.inline[c.kind] = append(st.inline[c.kind], c)
	}
	return nil
}

// project runs the inline handlers for recs, in the command's transaction.
func (p *Projector[K, E]) project(tx *writer.Tx, recs []Record[K, E]) error {
	for _, r := range recs {
		h, ok := p.handlers[r.Type]
		if !ok {
			return fmt.Errorf("events: projector %s has no handler for %s", p.core.name, r.Type)
		}
		if h.inline == nil {
			continue
		}
		if err := h.inline(tx, r); err != nil {
			return fmt.Errorf("events: projector %s on %s at %d: %w", p.core.name, r.Type, r.Position, err)
		}
	}
	return nil
}

func (p *Projector[K, E]) replay(st *Store, tx *writer.Tx, evs []Raw) error {
	recs := make([]Record[K, E], 0, len(evs))
	for _, ev := range evs {
		r, err := record(st, p.stream, ev)
		if err != nil {
			return err
		}
		recs = append(recs, r)
	}
	return p.project(tx, recs)
}

func (p *Projector[K, E]) deliver(ctx context.Context, st *Store, ev Raw) (err error) {
	r, err := record(st, p.stream, ev)
	if err != nil {
		return err
	}
	h, ok := p.handlers[r.Type]
	if !ok {
		return fmt.Errorf("events: projector %s has no handler for %s", p.core.name, r.Type)
	}
	if h.after == nil {
		return nil
	}
	defer func() {
		if v := recover(); v != nil {
			err = fmt.Errorf("events: projector %s panicked on %s at %d: %v\n%s", p.core.name, r.Type, r.Position, v, debug.Stack())
		}
	}()
	return h.after(ctx, r)
}

// record decodes a stored event of stream s, upcast.
func record[K Key, E any](st *Store, s *Stream[K, E], ev Raw) (Record[K, E], error) {
	key, err := keyFrom[K](ev.ID)
	if err != nil {
		return Record[K, E]{}, err
	}
	e, name, err := decode(st, s, ev.Type, ev.Data, false)
	if err != nil {
		return Record[K, E]{}, fmt.Errorf("events: event %d: %w", ev.Position, err)
	}
	return Record[K, E]{Position: ev.Position, ID: key, Version: ev.Version, Type: name, Data: e, Meta: ev.Meta}, nil
}
