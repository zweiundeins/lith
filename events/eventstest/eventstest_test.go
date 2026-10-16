package eventstest_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/events/eventstest"
	"github.com/zweiundeins/lith/writer"
)

type CounterEvent interface{ counterEvent() }

type Added struct {
	N int `json:"n"`
}

type Reset struct{}

func (Added) counterEvent() {}
func (Reset) counterEvent() {}

var Counters = events.NewStream[int64, CounterEvent]("counter")

func defineCounter[T CounterEvent](name string) events.Type[int64, CounterEvent, T] {
	return events.Define(Counters, name, func(t T) CounterEvent { return t })
}

var (
	_ = defineCounter[Added]("counter.added")
	_ = defineCounter[Reset]("counter.reset")
)

type counter struct{ Total int }

type Add struct{ N int }

var errTooMuch = errors.New("the counter is full")

var add = events.Decider[Add, CounterEvent, counter]{
	Decide: func(c Add, s counter) ([]CounterEvent, error) {
		switch {
		case c.N <= 0:
			return nil, &writer.Refusal{Msg: "Add a number above zero."}
		case s.Total+c.N > 10:
			return nil, fmt.Errorf("adding %d: %w", c.N, errTooMuch)
		}
		return []CounterEvent{Added{N: c.N}}, nil
	},
	Evolve: func(s counter, e CounterEvent) counter {
		switch e := e.(type) {
		case Added:
			s.Total += e.N
		case Reset:
			s.Total = 0
		}
		return s
	},
}

func TestGivenWhenThen(t *testing.T) {
	full := eventstest.For(add).Given(Added{N: 6}, Added{N: 3})
	full.When(Add{N: 1}).Then(t, Added{N: 1})
	full.When(Add{N: 2}).ThenError(t, errTooMuch)
	full.Given(Reset{}).When(Add{N: 10}).Then(t, Added{N: 10})
	eventstest.For(add).When(Add{N: 0}).ThenError(t, &writer.Refusal{Msg: "Add a number above zero."})
	if s := full.State(); s.Total != 9 {
		t.Errorf("state %+v", s)
	}
}

// recorder is a testing.TB that keeps what a test reported.
type recorder struct {
	testing.TB
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}
func (r *recorder) Helper() {}

type hidden struct{ total int }

// The checks fail where they should.
func TestFailures(t *testing.T) {
	r := &recorder{TB: t}
	eventstest.For(add).When(Add{N: 1}).Then(r, Added{N: 2})
	eventstest.For(add).When(Add{N: 1}).Then(r)
	eventstest.For(add).When(Add{N: 1}).ThenError(r, errTooMuch)
	eventstest.For(add).Given(Added{N: 10}).When(Add{N: 1}).ThenError(r, errors.New("another error"))
	if len(r.errs) != 4 {
		t.Errorf("reported %q", r.errs)
	}

	// A state with a field JSON does not keep decides differently once
	// the store has kept it.
	unkept := events.Decider[Add, CounterEvent, hidden]{
		Decide: func(c Add, s hidden) ([]CounterEvent, error) {
			if s.total+c.N > 10 {
				return nil, errTooMuch
			}
			return []CounterEvent{Added{N: c.N}}, nil
		},
		Evolve: func(s hidden, e CounterEvent) hidden { s.total += e.(Added).N; return s },
	}
	r = &recorder{TB: t}
	eventstest.For(unkept).Given(Added{N: 10}).When(Add{N: 1}).ThenError(r, errTooMuch)
	if len(r.errs) != 1 || !strings.Contains(r.errs[0], "JSON") {
		t.Errorf("reported %q", r.errs)
	}

	r = &recorder{TB: t}
	st := events.Must(events.New())
	eventstest.Samples(r, st, Counters, fstest.MapFS{"counter.added.json": {Data: []byte(`{"n": 1, "by": "ada"}`)}})
	if len(r.errs) != 2 || !strings.Contains(r.errs[0], `unknown field "by"`) || !strings.Contains(r.errs[1], "no sample of counter.reset") {
		t.Errorf("reported %q", r.errs)
	}
}
