// Package events keeps an event log in the app's SQLite database: typed
// streams of events, deciders that decide from each stream's own state,
// and projectors that keep read models current.
//
// It adds nothing between a handler and the writer. A command calls
// Execute or Append in its Apply, and the events, the stream's state row
// and the inline projections commit in the command's savepoint, or not at
// all. The writer applies one command at a time, so a stream's version
// cannot race, positions commit in the order they are handed out, and an
// inline projection is never behind its events.
//
// # Streams and event types
//
// A kind of stream has a key type, int64 or string, and an interface that
// its event types implement. Each event type is defined once, under the
// name the table stores, through a helper of one line that makes the
// compiler check that the type belongs to the stream:
//
//	type CardEvent interface{ cardEvent() }
//
//	type CardCreated struct {
//		Title string `json:"title"`
//	}
//
//	type CardRenamed struct {
//		From string `json:"from"`
//		To   string `json:"to"`
//	}
//
//	func (CardCreated) cardEvent() {}
//	func (CardRenamed) cardEvent() {}
//
//	var Cards = events.NewStream[int64, CardEvent]("card")
//
//	func defineCard[T CardEvent](name string) events.Type[int64, CardEvent, T] {
//		return events.Define(Cards, name, func(t T) CardEvent { return t })
//	}
//
//	var (
//		Created = defineCard[CardCreated]("card.created")
//		Renamed = defineCard[CardRenamed]("card.renamed")
//	)
//
// Events are stored as JSON. A new optional field needs nothing: old
// events decode without it. A change beyond that gets a new type, and an
// Upcaster reads the old one as the new.
//
// # Deciders and Execute
//
// A Decider is a stream kind's write side for a command type: Decide turns
// the command and the stream's state into events, Evolve folds an event
// into the state. The commands of one kind share the state, Evolve and
// Version, each with a Decide of its own. The store keeps each stream's
// state as JSON in a row, written in the same savepoint as the events, so
// a command reads one row instead of replaying the stream:
//
//	type card struct {
//		Exists bool
//		Title  string
//	}
//
//	var rename = events.Decider[Rename, CardEvent, card]{Decide: decideRename, Evolve: evolveCard, Version: 1}
//
//	func (c Rename) Apply(tx *writer.Tx) error {
//		return events.Execute(store, tx, rename, Cards.ID(c.Card), events.Exactly(c.Seen), c,
//			events.Meta{Actor: c.Session, Causation: c.Op})
//	}
//
// The expected version is the one the page showed when the person acted
// (Exactly), or Any, or NoStream for a stream that must not exist yet. A
// stream at another version fails the command with ErrConflict, and
// nothing changes.
//
// # Projectors
//
// A Projector keeps a read model current from one kind of stream. An
// inline projector's handlers (Handle) run in the command's transaction:
// pages read its tables in their next tick and never see an event without
// its effect. A catch-up projector's handlers (After) run after the
// commit, outside the writer, for mail, a search service or another
// system's API. A projector handles or ignores every event type of its
// stream, or New refuses it.
//
//	var cardTitles = events.NewProjector(Cards, "card_titles", 1,
//		events.Handle(Created, func(tx *writer.Tx, r events.Record[int64, CardCreated]) error {
//			_, err := tx.Exec(`INSERT INTO card_titles (id, title) VALUES (?, ?)`, r.ID, r.Data.Title)
//			return err
//		}),
//		events.Handle(Renamed, func(tx *writer.Tx, r events.Record[int64, CardRenamed]) error {
//			_, err := tx.Exec(`UPDATE card_titles SET title = ? WHERE id = ?`, r.Data.To, r.ID)
//			tx.Touch(CardTopic(r.ID))
//			return err
//		}),
//	).Tables("card_titles")
//
//	var store = events.Must(events.New(cardTitles))
//
// A Runner keeps the projectors current in a database. When an inline
// projector's version changes, it rebuilds the projector's tables: it
// fills a copy of each from every event, a command per batch, while pages
// read the old tables, which new events still reach, and swaps the copies
// in with the last batch. It
// runs the catch-up projectors from their checkpoints, woken by the
// commits that append their events; an event that keeps failing parks its
// projector, with the error in Stats.
//
// # Reads
//
// Load reads a stream's events and Since every event after a position,
// both as iterators over the transaction they are given. Pages render
// from projections: a stream's history only grows. LoadUntil, SinceUntil
// and StateAt read as of a position, for a page that shows the past.
//
// # Retention
//
// Forget deletes a stream's events and Prune a kind's older ones. A
// projection keeps what it built from them, but a rebuild after that sees
// only the events that are left: keep personal data out of events, or
// forget the whole stream with its rows in the projections.
//
// # Tables
//
// The first command that needs them creates three tables:
// lith_events (the position, from AUTOINCREMENT so that it is never
// reused, the stream's kind and key, its version, the type, the JSON
// data, the time in Unix milliseconds, the actor, correlation and
// causation), lith_event_state (each stream's folded state) and
// lith_projections (each projector's version, position and status).
// Positions commit in order, so a reader that keeps the last position it
// saw misses no event.
package events
