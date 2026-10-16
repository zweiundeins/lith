package events_test

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/events/eventstest"
	"github.com/zweiundeins/lith/writer"
)

// Notes went through three shapes of one event: a text, then a body with
// tags, then labels instead of tags. testdata/note keeps a sample of each
// as it was stored.

type NoteEvent interface{ noteEvent() }

type NoteAddedV1 struct {
	Text string `json:"text"`
}

type NoteAddedV2 struct {
	Body string   `json:"body"`
	Tags []string `json:"tags"`
}

type NoteAdded struct {
	Body   string   `json:"body"`
	Labels []string `json:"labels"`
}

func (NoteAddedV1) noteEvent() {}
func (NoteAddedV2) noteEvent() {}
func (NoteAdded) noteEvent()   {}

var Notes = events.NewStream[int64, NoteEvent]("note")

func defineNote[T NoteEvent](name string) events.Type[int64, NoteEvent, T] {
	return events.Define(Notes, name, func(t T) NoteEvent { return t })
}

var (
	AddedV1 = defineNote[NoteAddedV1]("note.added")
	AddedV2 = defineNote[NoteAddedV2]("note.added.v2")
	Added   = defineNote[NoteAdded]("note.added.v3")

	upcastV1 = events.Upcast(AddedV1, AddedV2, func(o NoteAddedV1) NoteAddedV2 { return NoteAddedV2{Body: o.Text} })
	upcastV2 = events.Upcast(AddedV2, Added, func(o NoteAddedV2) NoteAdded { return NoteAdded{Body: o.Body, Labels: o.Tags} })
)

// Every shape of a note decodes from its sample and reads as the newest.
func TestUpcastSamples(t *testing.T) {
	notes := events.NewProjector(Notes, "notes", 1, events.Ignore(Added))
	st := events.Must(events.New(notes, upcastV1, upcastV2))
	eventstest.Samples(t, st, Notes, os.DirFS("testdata/note"))

	for name, want := range map[string]NoteAdded{
		"note.added":    {Body: "Buy milk"},
		"note.added.v2": {Body: "Buy milk", Labels: []string{"home"}},
		"note.added.v3": {Body: "Call the bank", Labels: []string{"work", "today"}},
	} {
		data, err := os.ReadFile("testdata/note/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		got, err := events.Decode(st, Notes, name, data)
		if err != nil {
			t.Fatal(err)
		}
		if n, ok := got.(NoteAdded); !ok || n.Body != want.Body || !slices.Equal(n.Labels, want.Labels) {
			t.Errorf("%s reads as %#v", name, got)
		}
	}
	// A field renamed in place, instead of in a new type with an
	// upcaster, fails the sample's check.
	if err := events.CheckSample(st, Notes, "note.added", []byte(`{"text": "Buy milk"}`)); err != nil {
		t.Error(err)
	}
	if err := events.CheckSample(st, Notes, "note.added.v2", []byte(`{"text": "Buy milk"}`)); err == nil {
		t.Error("a sample with a field its type lacks passed")
	}
}

// Events stored in an old shape read as the newest through Load, while
// Since gives them as stored; a command can no longer append the old
// shape.
func TestUpcastOnRead(t *testing.T) {
	f := open(t)
	for i, e := range []NoteEvent{NoteAddedV1{Text: "one"}, NoteAddedV2{Body: "two", Tags: []string{"b"}}} {
		if err := f.do(t, func(tx *writer.Tx) error {
			return events.Append(f.st, tx, Notes.ID(1), events.Exactly(int64(i)), events.Meta{}, e)
		}); err != nil {
			t.Fatal(err)
		}
	}
	v3 := f.reopen(events.NewProjector(Notes, "notes", 1, events.Ignore(Added)), upcastV1, upcastV2)
	if err := v3.do(t, func(tx *writer.Tx) error {
		return events.Append(v3.st, tx, Notes.ID(1), events.Any, events.Meta{}, events.One[NoteEvent](NoteAddedV2{Body: "old"}))
	}); err == nil || !strings.Contains(err.Error(), "append that instead") {
		t.Errorf("an old shape appended: %v", err)
	}
	if err := v3.do(t, func(tx *writer.Tx) error {
		return events.Append(v3.st, tx, Notes.ID(1), events.Any, events.Meta{}, events.One[NoteEvent](NoteAdded{Body: "three"}))
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err := f.db.View(ctx, func(tx *sql.Tx) error {
		var bodies, types []string
		for r, err := range events.Load(ctx, v3.st, tx, Notes.ID(1), 0) {
			if err != nil {
				return err
			}
			bodies = append(bodies, r.Data.(NoteAdded).Body)
			types = append(types, r.Type)
		}
		if !slices.Equal(bodies, []string{"one", "two", "three"}) || !slices.Equal(types, []string{"note.added.v3", "note.added.v3", "note.added.v3"}) {
			t.Errorf("loaded %v %v", bodies, types)
		}
		var stored []string
		for raw, err := range events.Since(ctx, tx, 0) {
			if err != nil {
				return err
			}
			stored = append(stored, raw.Type)
		}
		if !slices.Equal(stored, []string{"note.added", "note.added.v2", "note.added.v3"}) {
			t.Errorf("stored %v", stored)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
