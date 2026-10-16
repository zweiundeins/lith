// Package eventstest tests deciders as given, when, then: the events a
// stream holds, a command, and the events or the error the command should
// cause. It runs the fold and the decision without a database.
//
//	func TestRename(t *testing.T) {
//		eventstest.For(rename).
//			Given(CardCreated{Title: "Draft"}).
//			When(Rename{Title: "Final"}).
//			Then(t, CardRenamed{From: "Draft", To: "Final"})
//	}
package eventstest

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/events"
)

// Case is a decider's test case. Given and When return a new Case, so a
// case can serve as the start of several.
type Case[C, E, S any] struct {
	d     events.Decider[C, E, S]
	given []E
	cmd   C
	when  bool
}

// For starts a test case of decider d.
func For[C, E, S any](d events.Decider[C, E, S]) Case[C, E, S] {
	return Case[C, E, S]{d: d}
}

// Given adds events that the stream holds before the command.
func (c Case[C, E, S]) Given(evs ...E) Case[C, E, S] {
	c.given = append(slices.Clip(c.given), evs...)
	return c
}

// When sets the command.
func (c Case[C, E, S]) When(cmd C) Case[C, E, S] {
	c.cmd, c.when = cmd, true
	return c
}

// State returns the state folded from the given events.
func (c Case[C, E, S]) State() S {
	s, err := c.fold()
	if err != nil {
		panic(err)
	}
	return s
}

func (c Case[C, E, S]) fold() (S, error) {
	var s S
	b, err := json.Marshal(c.d.Initial)
	if err != nil {
		return s, fmt.Errorf("the initial state: %w", err)
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("the initial state: %w", err)
	}
	for _, e := range c.given {
		s = c.d.Evolve(s, e)
	}
	return s, nil
}

// decide runs the command on the folded state, and again on that state
// as the store keeps it, in JSON: a decision that differs between the two
// depends on what the store does not keep, such as an unexported field.
func (c Case[C, E, S]) decide(t testing.TB) ([]E, error) {
	t.Helper()
	if !c.when {
		t.Fatal("eventstest: Then without When")
	}
	s, err := c.fold()
	if err != nil {
		t.Fatal(err)
	}
	evs, derr := c.d.Decide(c.cmd, s)
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("eventstest: the state does not encode: %v", err)
	}
	var stored S
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatalf("eventstest: the state does not decode: %v", err)
	}
	evs2, derr2 := c.d.Decide(c.cmd, stored)
	if !reflect.DeepEqual(evs, evs2) || fmt.Sprint(derr) != fmt.Sprint(derr2) {
		t.Errorf("eventstest: the decision on the state read back from JSON differs, %s and %v, from the one on the state folded, %s and %v: does the state have fields JSON does not keep?",
			show(evs2), derr2, show(evs), derr)
	}
	return evs, derr
}

// Then checks that the command causes exactly the events want, in order,
// and no error. Then(t) checks that it causes nothing.
func (c Case[C, E, S]) Then(t testing.TB, want ...E) {
	t.Helper()
	got, err := c.decide(t)
	if err != nil {
		t.Fatalf("eventstest: %T refused: %v", c.cmd, err)
	}
	if (len(got) > 0 || len(want) > 0) && !reflect.DeepEqual(got, want) {
		t.Errorf("eventstest: %T caused\n%s\nwant\n%s", c.cmd, show(got), show(want))
	}
}

// ThenError checks that the command fails with want: an error that
// errors.Is matches, or one with the same message, such as a
// *writer.Refusal.
func (c Case[C, E, S]) ThenError(t testing.TB, want error) {
	t.Helper()
	got, err := c.decide(t)
	switch {
	case err == nil:
		t.Errorf("eventstest: %T caused %s, want the error %v", c.cmd, show(got), want)
	case !errors.Is(err, want) && (want == nil || err.Error() != want.Error()):
		t.Errorf("eventstest: %T failed with %v, want %v", c.cmd, err, want)
	case len(got) > 0:
		t.Errorf("eventstest: %T failed with %v and also returned %s", c.cmd, err, show(got))
	}
}

func show[E any](evs []E) string {
	if len(evs) == 0 {
		return "\tno events"
	}
	var b strings.Builder
	for i, e := range evs {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "\t%T %+v", e, e)
	}
	return b.String()
}

// Samples checks the JSON samples of a stream's event types in fsys, a
// file per type named after it, such as card.renamed.json: every type of
// the stream has one, and each decodes, without a field its type lacks,
// and upcasts (events.CheckSample). Keep a sample of every type as it was
// first stored, and never change one: a field renamed instead of upcast
// then fails the test instead of the reads in production.
func Samples[K events.Key, E any](t testing.TB, st *events.Store, s *events.Stream[K, E], fsys fs.FS) {
	t.Helper()
	for _, name := range s.Types() {
		data, err := fs.ReadFile(fsys, name+".json")
		if err != nil {
			t.Errorf("eventstest: no sample of %s: %v", name, err)
			continue
		}
		if err := events.CheckSample(st, s, name, data); err != nil {
			t.Errorf("eventstest: the sample of %s: %v", name, err)
		}
	}
}
