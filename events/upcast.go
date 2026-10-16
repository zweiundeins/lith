package events

import (
	"errors"
	"fmt"
)

// Upcaster turns a stored event of an older type into the newer type as
// it is read. Make it with Upcast and pass it to New.
type Upcaster struct {
	kind     string
	stream   any
	from, to string
	fn       func(any) any
}

// Upcast reads every stored event of type from as an event of type to,
// converted by fn. Stored events are never changed: an event shape that
// needs more than a new optional field (which old events decode without)
// gets a new type, and the old type stays defined for the events stored
// with it. Projectors and deciders then see only the new type, and a
// command can no longer append the old one. Upcasters chain: from v1 to
// v2 and from v2 to v3 reads v1 as v3.
func Upcast[K Key, E, Old, New any](from Type[K, E, Old], to Type[K, E, New], fn func(Old) New) Upcaster {
	return Upcaster{kind: from.s.kind, stream: from.s, from: from.name, to: to.name, fn: func(v any) any { return fn(v.(Old)) }}
}

// From returns the name of the type it reads.
func (u Upcaster) From() string { return u.from }

// To returns the name of the type it turns it into.
func (u Upcaster) To() string { return u.to }

func (u Upcaster) add(st *Store) error {
	if u.fn == nil {
		return errors.New("events: an Upcaster made without Upcast")
	}
	if u.from == u.to {
		return fmt.Errorf("events: %s is upcast to itself", u.from)
	}
	if err := st.stream(u.kind, u.stream); err != nil {
		return err
	}
	ups := st.upcasts[u.kind]
	if ups == nil {
		ups = map[string]*Upcaster{}
		st.upcasts[u.kind] = ups
	}
	if ups[u.from] != nil {
		return fmt.Errorf("events: %s is upcast twice", u.from)
	}
	ups[u.from] = &u
	return nil
}

// decode decodes data as an event of type name and upcasts it. It returns
// the event and the name of its type.
func decode[K Key, E any](st *Store, s *Stream[K, E], name string, data []byte, strict bool) (E, string, error) {
	var zero E
	t := s.types[name]
	if t == nil {
		return zero, name, fmt.Errorf("events: %s is not an event type of stream %s", name, s.kind)
	}
	v, err := t.decode(data, strict)
	if err != nil {
		return zero, name, fmt.Errorf("events: decode %s: %w", name, err)
	}
	if st != nil {
		for up := st.upcasts[s.kind][name]; up != nil; up = st.upcasts[s.kind][name] {
			v, name = up.fn(v), up.to
		}
		t = s.types[name]
	}
	return t.as(v), name, nil
}

// Decode decodes data as an event of type name of stream s, upcast to the
// newest type, as Load does: for the events Since reads, say.
func Decode[K Key, E any](st *Store, s *Stream[K, E], name string, data []byte) (E, error) {
	e, _, err := decode(st, s, name, data, false)
	return e, err
}

// CheckSample decodes a JSON sample of an event as Decode does, but
// refuses a field that the type of name lacks: a test that keeps a sample
// of each type, as it was stored, fails when a field is renamed instead
// of upcast.
func CheckSample[K Key, E any](st *Store, s *Stream[K, E], name string, data []byte) error {
	_, _, err := decode(st, s, name, data, true)
	return err
}
