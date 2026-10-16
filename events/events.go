package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"time"
)

// Key is the type of a stream's ids: an integer or a text, which the
// tables store and index as they are.
type Key interface{ ~int64 | ~string }

// Stream is a kind of stream, such as "card": the type of its ids and of
// its events, and the event types defined on it. Make one per kind at
// package level with NewStream, and define its event types with Define.
type Stream[K Key, E any] struct {
	kind  string
	types map[string]*eventType[E]
	names map[reflect.Type]string
	order []string
}

// eventType is an event type of a stream, with its Go type erased: decode
// returns a T, and as turns a T into the stream's E.
type eventType[E any] struct {
	name   string
	decode func(data []byte, strict bool) (any, error)
	as     func(any) E
}

// NewStream makes a kind of stream. The kind is what the tables store in
// their stream column, so it must not change once events are stored.
func NewStream[K Key, E any](kind string) *Stream[K, E] {
	if kind == "" {
		panic("events: a stream needs a kind")
	}
	return &Stream[K, E]{kind: kind, types: map[string]*eventType[E]{}, names: map[reflect.Type]string{}}
}

// Kind returns the stream's kind.
func (s *Stream[K, E]) Kind() string { return s.kind }

// Types returns the names of the event types defined on the stream, in
// the order they were defined.
func (s *Stream[K, E]) Types() []string { return slices.Clone(s.order) }

// ID names one stream of the kind.
func (s *Stream[K, E]) ID(key K) StreamID[K, E] { return StreamID[K, E]{s: s, key: key} }

// StreamID names one stream: its kind and its key.
type StreamID[K Key, E any] struct {
	s   *Stream[K, E]
	key K
}

// Key returns the stream's key.
func (id StreamID[K, E]) Key() K { return id.key }

// Stream returns the stream's kind.
func (id StreamID[K, E]) Stream() *Stream[K, E] { return id.s }

// Type is an event type of a stream: the Go type T, stored under a name.
type Type[K Key, E, T any] struct {
	s    *Stream[K, E]
	name string
}

// Name returns the name the tables store the type under.
func (t Type[K, E, T]) Name() string { return t.name }

// Define defines T as an event type of s, stored under name. as converts
// a T to the stream's event type; passing it makes the compiler check
// that T is one. Go cannot constrain T by E, so each stream gets a helper
// of one line that does:
//
//	func defineCard[T CardEvent](name string) events.Type[int64, CardEvent, T] {
//		return events.Define(Cards, name, func(t T) CardEvent { return t })
//	}
//
//	var Renamed = defineCard[CardRenamed]("card.renamed")
//
// Define panics when the name or T is defined on s already: define each
// type once, at package level.
func Define[K Key, E, T any](s *Stream[K, E], name string, as func(T) E) Type[K, E, T] {
	if name == "" {
		panic("events: an event type needs a name")
	}
	if _, dup := s.types[name]; dup {
		panic(fmt.Sprintf("events: %s is defined twice on stream %s", name, s.kind))
	}
	rt := reflect.TypeFor[T]()
	if other, dup := s.names[rt]; dup {
		panic(fmt.Sprintf("events: %s is defined on stream %s as %s and as %s", rt, s.kind, other, name))
	}
	s.types[name] = &eventType[E]{
		name: name,
		decode: func(data []byte, strict bool) (any, error) {
			var t T
			if !strict {
				err := json.Unmarshal(data, &t)
				return t, err
			}
			d := json.NewDecoder(bytes.NewReader(data))
			d.DisallowUnknownFields()
			err := d.Decode(&t)
			return t, err
		},
		as: func(v any) E { return as(v.(T)) },
	}
	s.names[rt] = name
	s.order = append(s.order, name)
	return Type[K, E, T]{s: s, name: name}
}

// nameOf returns the name e's type is defined under.
func (s *Stream[K, E]) nameOf(e E) (string, error) {
	rt := reflect.TypeOf(any(e))
	if rt == nil {
		return "", fmt.Errorf("events: a nil event for stream %s", s.kind)
	}
	name, ok := s.names[rt]
	if !ok {
		return "", fmt.Errorf("events: %s is not an event type of stream %s", rt, s.kind)
	}
	return name, nil
}

// One returns e as the stream's event type E, for Append with a single
// event of a concrete type: Go infers E from the stream's id and then
// refuses a struct where it expects E.
//
//	events.Append(st, tx, Cards.ID(7), events.Any, meta, events.One[CardEvent](CardRenamed{To: "Ship it"}))
func One[E any](e E) E { return e }

// Meta is what an event records besides its data.
type Meta struct {
	// At is when the event was appended, stored in Unix milliseconds.
	// Append and Execute take the time then when it is zero; an import
	// sets it to when the event happened.
	At time.Time
	// Actor is who caused the event: a user, a session.
	Actor string
	// Correlation names what the event belongs to: a request, a job.
	Correlation string
	// Causation names what caused it: the command's op id, or the
	// position of the event a catch-up projector reacted to.
	Causation string
}

// Record is a stored event of a stream with ids of type K, its data a T.
type Record[K Key, T any] struct {
	Position int64  // in the store, across streams; never reused
	ID       K      // the stream's key
	Version  int64  // in its stream, from 1
	Type     string // the name of its event type, after upcasting
	Data     T
	Meta
}

// Raw is a stored event as the table holds it, for reads across streams.
type Raw struct {
	Position int64
	Stream   string          // the stream's kind
	ID       any             // the stream's key: an int64 or a string
	Version  int64           // in its stream, from 1
	Type     string          // the name it was stored under, before upcasting
	Data     json.RawMessage // its JSON
	Meta
}

// Expected is the version a stream must be at for an append to go ahead:
// Any, NoStream or Exactly. A stream's version is the number of events
// appended to it.
type Expected struct {
	set     bool
	version int64
}

var (
	// Any appends at whatever version the stream is.
	Any = Expected{}
	// NoStream appends only to a stream without events.
	NoStream = Expected{set: true}
)

// Exactly appends only while the stream is at version v: the version the
// page showed when the person acted. Exactly(0) is NoStream.
func Exactly(v int64) Expected { return Expected{set: true, version: v} }

// ErrConflict is the error, wrapped, of an append to a stream that is not
// at the expected version. Nothing was appended.
var ErrConflict = errors.New("events: the stream is not at the expected version")

func (x Expected) check(kind string, key any, version int64) error {
	if !x.set || x.version == version {
		return nil
	}
	return fmt.Errorf("%w: stream %s %v is at version %d, not %d", ErrConflict, kind, key, version, x.version)
}

// keyValue returns k as the int64 or string the driver binds.
func keyValue[K Key](k K) any {
	switch v := any(k).(type) {
	case int64:
		return v
	case string:
		return v
	}
	if rv := reflect.ValueOf(k); rv.Kind() == reflect.String {
		return rv.String()
	} else {
		return rv.Int()
	}
}

// keyFrom converts a stored key back to K.
func keyFrom[K Key](v any) (K, error) {
	var k K
	rv := reflect.ValueOf(&k).Elem()
	switch v := v.(type) {
	case int64:
		if rv.Kind() == reflect.Int64 {
			rv.SetInt(v)
			return k, nil
		}
	case string:
		if rv.Kind() == reflect.String {
			rv.SetString(v)
			return k, nil
		}
	}
	return k, fmt.Errorf("events: a stored key %v (%T) is not a %T", v, v, k)
}
