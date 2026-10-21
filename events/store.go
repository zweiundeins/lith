package events

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"sync"

	"github.com/zweiundeins/lith/writer"
)

// Topic is touched by every command that appends or deletes events: a
// view over Since, such as an activity feed, lists it.
const Topic = "lith:events"

// The module's tables, created in the first command that needs them, as
// writer.Once creates lith_ops. Positions use AUTOINCREMENT, so a position
// is never handed out twice, also once retention deleted the newest
// events.
var schema = []string{
	`CREATE TABLE IF NOT EXISTS lith_events (
	position    INTEGER PRIMARY KEY AUTOINCREMENT,
	stream      TEXT NOT NULL,
	stream_id   ANY NOT NULL,
	version     INTEGER NOT NULL,
	type        TEXT NOT NULL,
	data        TEXT NOT NULL,
	at          INTEGER NOT NULL,
	actor       TEXT,
	correlation TEXT,
	causation   TEXT,
	UNIQUE (stream, stream_id, version)
) STRICT`,
	`CREATE INDEX IF NOT EXISTS lith_events_stream ON lith_events (stream, position)`,
	`CREATE TABLE IF NOT EXISTS lith_event_state (
	stream    TEXT NOT NULL,
	stream_id ANY NOT NULL,
	version   INTEGER NOT NULL,
	fold      INTEGER NOT NULL,
	state     TEXT NOT NULL,
	PRIMARY KEY (stream, stream_id)
) STRICT, WITHOUT ROWID`,
	`CREATE TABLE IF NOT EXISTS lith_projections (
	name     TEXT PRIMARY KEY,
	version  INTEGER NOT NULL,
	position INTEGER NOT NULL,
	status   TEXT NOT NULL
) STRICT`,
	// Every event of a kind stored at or before its compacted position is
	// a snapshot (Compact).
	`CREATE TABLE IF NOT EXISTS lith_event_kinds (
	stream    TEXT PRIMARY KEY,
	compacted INTEGER NOT NULL
) STRICT, WITHOUT ROWID`,
}

// A projector's status in lith_projections.
const (
	statusLive       = "live"
	statusRebuilding = "rebuilding"
	parkedPrefix     = "parked: "
)

// Store holds the projectors and upcasters of an app's event streams. It
// keeps no connection: Execute and Append run in a command's transaction,
// Load and Since in the transaction they are given. One Store serves
// every database the app opens, each with its writer, and keeps what it
// learns of a database on that writer (writer.Tx.Local).
type Store struct {
	projectors []*projector
	byName     map[string]*projector
	inline     map[string][]*projector // by stream kind
	catchUp    map[string][]*projector
	upcasts    map[string]map[string]*Upcaster // by stream kind, then the old type's name
	streams    map[string]any                  // the *Stream of each kind New saw
	owners     map[string]string               // the projector of each table, by its name in lower case

	once     sync.Once
	checkErr error
	states   sync.Map // stream kind: stateType, the deciders' state seen first
	counts   counts
	scanAll  bool // tests: cut ignores its marks
}

// Part is what New takes: a *Projector or an Upcaster.
type Part interface {
	add(st *Store) error
}

// New makes a store of the projectors and upcasters. It refuses a
// projector that neither handles nor ignores an event type of its stream,
// two projectors with one name or one table, two streams with one kind,
// and an event type upcast twice. The first Execute, Append or Run checks once more,
// for event types defined after New ran.
func New(parts ...Part) (*Store, error) {
	st := &Store{
		byName:  map[string]*projector{},
		inline:  map[string][]*projector{},
		catchUp: map[string][]*projector{},
		upcasts: map[string]map[string]*Upcaster{},
		streams: map[string]any{},
		owners:  map[string]string{},
	}
	var errs []error
	for _, p := range parts {
		if p == nil {
			errs = append(errs, errors.New("events: New got a nil part"))
			continue
		}
		if err := p.add(st); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, st.check())
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return st, nil
}

// Must returns st, and panics if err is not nil: for a store made at
// package level, events.Must(events.New(...)).
func Must(st *Store, err error) *Store {
	if err != nil {
		panic(err)
	}
	return st
}

// stream records s as the stream of its kind, refusing a second stream of
// the same kind.
func (st *Store) stream(kind string, s any) error {
	if other, ok := st.streams[kind]; ok && other != s {
		return fmt.Errorf("events: two streams of kind %s", kind)
	}
	st.streams[kind] = s
	return nil
}

// check runs the checks that depend on the event types defined so far.
func (st *Store) check() error {
	var errs []error
	for _, p := range st.projectors {
		for _, name := range p.types() {
			if st.upcasts[p.kind][name] != nil {
				if p.handled[name] {
					errs = append(errs, fmt.Errorf("events: projector %s handles %s, which is upcast to a newer type on reading", p.name, name))
				}
				continue
			}
			if !p.handled[name] {
				errs = append(errs, fmt.Errorf("events: projector %s neither handles nor ignores %s", p.name, name))
			}
		}
	}
	for kind, ups := range st.upcasts {
		for from := range ups {
			seen := map[string]bool{}
			for name := from; ups[name] != nil; name = ups[name].to {
				if seen[name] {
					errs = append(errs, fmt.Errorf("events: the upcasters of stream %s go round in a circle at %s", kind, from))
					break
				}
				seen[name] = true
			}
		}
	}
	return errors.Join(errs...)
}

// ready checks the store once more at its first use, when every package
// has defined its event types.
func (st *Store) ready() error {
	st.once.Do(func() { st.checkErr = st.check() })
	return st.checkErr
}

// stateType is the state type and version of a stream kind's deciders.
type stateType struct {
	t       reflect.Type
	version int
}

// checkDecider refuses a decider whose state differs from the one the
// stream kind's state rows were seen with first: they share one row per
// stream.
func checkDecider[S any](st *Store, kind string, version int) error {
	want := stateType{reflect.TypeFor[S](), version}
	got, _ := st.states.LoadOrStore(kind, want)
	if got != want {
		g := got.(stateType)
		return fmt.Errorf("events: the deciders of stream %s differ: state %s version %d, and %s version %d", kind, g.t, g.version, want.t, want.version)
	}
	return nil
}

// local is what the store knows of one writer's database.
type local struct {
	// broken holds, by stream kind, why its projectors cannot run, such
	// as a missing table: commands on that kind fail with it.
	broken map[string]error
	log    *slog.Logger // the runner's, nil without one

	ready bool            // the tables exist, committed
	live  map[string]bool // inline projectors that run, by name; nil until known
	gen   int             // counts the changes of live
	wake  map[string]func()
	heads *heads // the runner's, nil without one

	marks map[markKey]mark // cut's Since lookups, committed
}

func (st *Store) local(tx *writer.Tx) *local {
	if l, ok := tx.Local(st).(*local); ok {
		return l
	}
	l := &local{}
	tx.SetLocal(st, l)
	return l
}

func (l *local) logger() *slog.Logger {
	if l.log != nil {
		return l.log
	}
	return slog.Default()
}

// forget drops what l knows of the projectors' statuses: the next command
// reads them from the database. A command that changes a status calls it
// before the change.
func (l *local) forget() {
	l.live = nil
	l.gen++
}

// tables creates the module's tables, the first time on each writer.
func (st *Store) tables(tx *writer.Tx, l *local) error {
	if l.ready {
		return nil
	}
	for _, q := range schema {
		if _, err := tx.ExecUncached(q); err != nil {
			return fmt.Errorf("events: create the tables: %w", err)
		}
	}
	tx.AfterCommit(func() { l.ready = true })
	return nil
}

// running returns the inline projectors whose table is live, not
// rebuilding, and by stream kind why a kind's projectors cannot run. It
// records new projectors and changed versions in lith_projections. What
// it finds is kept for the commands that follow once the batch has
// committed, unless a status changed meanwhile.
func (st *Store) running(tx *writer.Tx, l *local) (map[string]bool, map[string]error, error) {
	if l.live != nil {
		return l.live, l.broken, nil
	}
	gen := l.gen
	live := map[string]bool{}
	var broken map[string]error
	for _, p := range st.projectors {
		status, err := st.status(tx, p)
		var missing *noTableError
		if errors.As(err, &missing) {
			if broken == nil {
				broken = map[string]error{}
			}
			broken[p.kind] = errors.Join(broken[p.kind], err)
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		if !p.catchUp && status == statusLive {
			live[p.name] = true
		}
	}
	tx.AfterCommit(func() {
		if l.gen == gen {
			l.live, l.broken = live, broken
		}
	})
	return live, broken, nil
}

// status returns p's status, recording p when it is new and its version
// when it changed.
func (st *Store) status(tx *writer.Tx, p *projector) (string, error) {
	var version int
	var status string
	err := tx.QueryRow(`SELECT version, status FROM lith_projections WHERE name = ?`, p.name).Scan(&version, &status)
	switch {
	case err == nil && version == p.version:
		return status, nil
	case err != nil && !errors.Is(err, sql.ErrNoRows):
		return "", err
	}
	isNew := err != nil
	status, position := statusLive, int64(0)
	switch {
	case p.catchUp && p.skipHistory:
		if err := tx.QueryRow(`SELECT coalesce(max(position), 0) FROM lith_events WHERE stream = ?`, p.kind).Scan(&position); err != nil {
			return "", err
		}
	case p.catchUp, len(p.tables) == 0:
	default:
		if err := st.checkTables(tx, p); err != nil {
			return "", err
		}
		if isNew {
			var n int
			err := tx.QueryRow(`SELECT count(*) FROM (SELECT 1 FROM lith_events WHERE stream = ? LIMIT 1)`, p.kind).Scan(&n)
			if err != nil {
				return "", err
			}
			if n == 0 {
				break
			}
		}
		status = statusRebuilding
	}
	if isNew {
		_, err = tx.Exec(`INSERT INTO lith_projections (name, version, position, status) VALUES (?, ?, ?, ?)`, p.name, p.version, position, status)
	} else {
		_, err = tx.Exec(`UPDATE lith_projections SET version = ?, position = ?, status = ? WHERE name = ?`, p.version, position, status, p.name)
	}
	return status, err
}

// noTableError is a projector's table that does not exist.
type noTableError struct{ projector, table string }

func (e *noTableError) Error() string {
	return fmt.Sprintf("events: projector %s has no table %s: create it in a migration", e.projector, e.table)
}

// checkTables reports a table of p that does not exist.
func (st *Store) checkTables(tx *writer.Tx, p *projector) error {
	for _, t := range p.tables {
		var n int
		err := tx.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ? COLLATE NOCASE`, t).Scan(&n)
		if err != nil {
			return err
		}
		if n == 0 {
			return &noTableError{p.name, t}
		}
	}
	return nil
}

// prepare makes the tables and returns the inline projectors that run,
// or why the projectors of kind cannot run. An empty kind checks none.
func (st *Store) prepare(tx *writer.Tx, kind string) (*local, map[string]bool, error) {
	if err := st.ready(); err != nil {
		return nil, nil, err
	}
	l := st.local(tx)
	if err := st.tables(tx, l); err != nil {
		return nil, nil, err
	}
	live, broken, err := st.running(tx, l)
	if err == nil {
		err = broken[kind]
	}
	return l, live, err
}
