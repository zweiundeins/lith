package board_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/events/eventstest"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/docs/example/board"
	"github.com/zweiundeins/lith/site/internal/docs/example/board/migrations"
)

var now = time.Unix(1_800_000_000, 0)

// open gives a migrated database with board 1 (lanes 1 and 2, two cards
// at most in lane 2), a running writer, and the topics its last batches
// touched.
func open(t *testing.T) (*sqlite.DB, *writer.Writer, func() []string) {
	t.Helper()
	db, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "board.db"), sqlite.Options{Migrations: migrations.FS})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var topics []string
	w, err := writer.New(db, writer.Options{OnCommit: func(ts []string) {
		mu.Lock()
		topics = append(topics, ts...)
		mu.Unlock()
	}})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(context.Background())
	t.Cleanup(func() {
		w.Close()
		db.Close()
	})
	seed := writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`INSERT INTO boards (id, name) VALUES (1, 'Launch');
			INSERT INTO lanes (id, board_id, name) VALUES (1, 1, 'To do');
			INSERT INTO lanes (id, board_id, name, max_cards) VALUES (2, 1, 'Doing', 2)`)
		return err
	})
	if err := w.Do(context.Background(), seed); err != nil {
		t.Fatal(err)
	}
	touched := func() []string {
		mu.Lock()
		defer mu.Unlock()
		got := topics
		topics = nil
		slices.Sort(got)
		return slices.Compact(got)
	}
	return db, w, touched
}

func load(t *testing.T, db *sqlite.DB) board.Board {
	t.Helper()
	ctx := context.Background()
	b, err := db.ViewValue(ctx, func(tx *sql.Tx) (board.Board, error) { return board.Load(ctx, tx, 1) })
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAddCard(t *testing.T) {
	db, w, touched := open(t)
	ctx := context.Background()

	add := board.AddCard{Lane: 2, Title: " Write the docs "}
	if err := add.Validate(); err != nil {
		t.Fatal(err)
	}
	id, err := w.DoValue(ctx, add.Insert)
	if err != nil || id != 1 {
		t.Fatalf("DoValue: %d, %v", id, err)
	}
	if got := touched(); !slices.Equal(got, []string{"board:1"}) {
		t.Errorf("touched %v", got)
	}
	if err := w.Do(ctx, board.AddCard{Lane: 2, Title: "Review them"}); err != nil {
		t.Fatal(err)
	}
	var refusal *writer.Refusal
	if err := w.Do(ctx, board.AddCard{Lane: 2, Title: "One too many"}); !errors.As(err, &refusal) || refusal.Msg != "This lane is full." {
		t.Errorf("a full lane: %v", err)
	}
	if err := w.Do(ctx, board.AddCard{Lane: 2, Title: ""}); !errors.Is(err, writer.ErrInvalid) {
		t.Errorf("an empty title: %v", err)
	}
	if b := load(t, db); len(b.Lanes[1].Cards) != 2 || b.Lanes[1].Cards[0].Title != "Write the docs" {
		t.Errorf("board %+v", b)
	}
}

func TestOnceResult(t *testing.T) {
	_, w, _ := open(t)
	ctx := context.Background()
	first := writer.OnceResult("s1:op1", board.AddCard{Lane: 1, Title: "Once"})
	if err := w.Do(ctx, first); err != nil || first.Replayed() || string(first.Result()) != "1" {
		t.Fatalf("first: %v %v %q", err, first.Replayed(), first.Result())
	}
	retry := writer.OnceResult("s1:op1", board.AddCard{Lane: 1, Title: "Once"})
	if err := w.Do(ctx, retry); err != nil || !retry.Replayed() || string(retry.Result()) != "1" {
		t.Fatalf("retry: %v %v %q", err, retry.Replayed(), retry.Result())
	}
	for range 2 {
		if err := w.Do(ctx, board.AddCard{Lane: 2, Title: "Fill"}); err != nil {
			t.Fatal(err)
		}
	}
	var refusal *writer.Refusal
	for range 2 { // the refusal is recorded: the retry gets it without running
		op := writer.OnceResult("s1:op2", board.AddCard{Lane: 2, Title: "Full"})
		if err := w.Do(ctx, op); !errors.As(err, &refusal) || refusal.Msg != "This lane is full." {
			t.Fatalf("refusal: %v", err)
		}
	}
}

func TestAct(t *testing.T) {
	db, w, touched := open(t)
	ctx := context.Background()
	add := board.AddCard{Lane: 2, Title: "Card"}
	for i, op := range []string{"op1", "op2", "op3"} {
		act := writer.Once("s1:"+op, board.Act{Session: "s1", Tab: "t1", Key: "add", Op: op, Cmd: add, At: now})
		if err := w.Do(ctx, act); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	state, err := db.ViewValue(ctx, func(tx *sql.Tx) (board.TabState, error) { return board.Tab(ctx, tx, "s1", "t1") })
	if err != nil || state.Ops["add"] != (board.Ack{Op: "op3", Refused: true}) || state.Refusal != "This lane is full." {
		t.Fatalf("tab %+v %v", state, err)
	}
	if got := touched(); !slices.Equal(got, []string{"board:1", "tab:t1"}) {
		t.Errorf("touched %v", got)
	}
	// A retry of op3 runs nothing: Once recorded it with its outcome.
	if err := w.Do(ctx, writer.Once("s1:op3", board.Act{Session: "s1", Tab: "t1", Key: "add", Op: "op3", Cmd: add, At: now})); err != nil {
		t.Fatal(err)
	}
	if got := touched(); len(got) != 0 {
		t.Errorf("a retry touched %v", got)
	}

	// What the person typed fails the command's Validate: a refusal in the
	// tab, not an error of Do.
	act := board.Act{Session: "s1", Tab: "t1", Key: "add", Op: "op4", Cmd: board.AddCard{Lane: 1, Title: " "}, At: now}
	if err := w.Do(ctx, writer.Once("s1:op4", act)); err != nil {
		t.Fatal(err)
	}
	state, err = db.ViewValue(ctx, func(tx *sql.Tx) (board.TabState, error) { return board.Tab(ctx, tx, "s1", "t1") })
	if err != nil || state.Ops["add"] != (board.Ack{Op: "op4", Refused: true}) || state.Refusal != "A title has 1 to 200 characters." {
		t.Fatalf("tab %+v %v", state, err)
	}
	if got := touched(); !slices.Equal(got, []string{"tab:t1"}) {
		t.Errorf("a refusal touched %v", got)
	}

	// Each key keeps its newest op; the row keeps the newest MaxKeys keys.
	for i := range board.MaxKeys + 2 {
		key := fmt.Sprintf("card-%d", i)
		cmd := writer.Func(func(*writer.Tx) error { return nil })
		if err := w.Do(ctx, board.Act{Session: "s1", Tab: "t1", Key: key, Op: fmt.Sprintf("op9%02d", i), Cmd: cmd, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	state, err = db.ViewValue(ctx, func(tx *sql.Tx) (board.TabState, error) { return board.Tab(ctx, tx, "s1", "t1") })
	if err != nil || len(state.Ops) != board.MaxKeys || state.Ops["card-17"].Op != "op917" || state.Ops["add"].Op != "" {
		t.Fatalf("tab after many keys: %+v %v", state, err)
	}
}

func TestMoveCard(t *testing.T) {
	db, w, touched := open(t)
	ctx := context.Background()
	if err := w.Do(ctx, board.AddCard{Lane: 1, Title: "Card"}); err != nil {
		t.Fatal(err)
	}
	touched()
	if err := w.Do(ctx, board.MoveCard{Card: 1, Lane: 2, Seen: 1}); err != nil {
		t.Fatal(err)
	}
	if got := touched(); !slices.Equal(got, []string{"board:1", "card:1"}) {
		t.Errorf("touched %v", got)
	}
	var refusal *writer.Refusal
	if err := w.Do(ctx, board.MoveCard{Card: 1, Lane: 1, Seen: 1}); !errors.As(err, &refusal) {
		t.Errorf("a stale move: %v", err)
	}
	if err := w.Do(ctx, board.MoveCard{Card: 1, Lane: 9, Seen: 2}); !errors.As(err, &refusal) || refusal.Msg != "This lane is gone." {
		t.Errorf("a move to a lane of no board: %v", err)
	}
	if err := w.Do(ctx, board.MoveCard{Card: 7, Lane: 1, Seen: 1}); !errors.As(err, &refusal) || refusal.Msg != "This card is gone." {
		t.Errorf("a move of a card that is gone: %v", err)
	}
	if b := load(t, db); len(b.Lanes[1].Cards) != 1 || b.Lanes[1].Cards[0].Version != 2 {
		t.Errorf("board %+v", b)
	}
}

func TestArchiveAndSweep(t *testing.T) {
	db, w, _ := open(t)
	ctx := context.Background()
	for _, title := range []string{"a", "b", "c"} {
		if err := w.Do(ctx, board.AddCard{Lane: 1, Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Do(ctx, board.RenameCard{Card: 1, Title: "A", At: now}); err != nil {
		t.Fatal(err)
	}
	if err := w.Do(ctx, board.ArchiveCards{Board: 1, Cards: []int64{1, 2}, At: now}); err != nil {
		t.Fatal(err)
	}
	if b := load(t, db); len(b.Lanes[0].Cards) != 1 {
		t.Errorf("after archiving: %+v", b.Lanes[0])
	}
	if err := w.Do(ctx, board.Sweep{At: now.AddDate(0, 2, 0)}); err != nil {
		t.Fatal(err)
	}
	n, err := db.ViewValue(ctx, func(tx *sql.Tx) (n int, err error) {
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM cards`).Scan(&n)
		return n, err
	})
	if err != nil || n != 1 {
		t.Errorf("after the sweep: %d cards, %v", n, err)
	}
	n, err = db.ViewValue(ctx, func(tx *sql.Tx) (n int, err error) {
		err = tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM lith_events) + (SELECT count(*) FROM card_history)`).Scan(&n)
		return n, err
	})
	if err != nil || n != 0 {
		t.Errorf("after the sweep: %d events and titles of archived cards, %v", n, err)
	}
}

func TestRenameCard(t *testing.T) {
	db, w, touched := open(t)
	ctx := context.Background()
	if err := w.Do(ctx, board.AddCard{Lane: 1, Title: "Draft"}); err != nil {
		t.Fatal(err)
	}
	touched()
	for _, title := range []string{"Final", "Final", "Shipped"} {
		if err := w.Do(ctx, board.RenameCard{Card: 1, Title: title, At: now}); err != nil {
			t.Fatal(err)
		}
	}
	var refusal *writer.Refusal
	if err := w.Do(ctx, board.RenameCard{Card: 1, Title: " ", At: now}); !errors.As(err, &refusal) {
		t.Errorf("an empty title: %v", err)
	}
	if err := w.Do(ctx, board.RenameCard{Card: 9, Title: "x", At: now}); !errors.Is(err, board.ErrNotFound) {
		t.Errorf("a rename of no card: %v", err)
	}
	if got := touched(); !slices.Equal(got, []string{"board:1", "card:1", events.Topic}) {
		t.Errorf("touched %v", got)
	}
	hist, err := db.ViewValue(ctx, func(tx *sql.Tx) ([]board.Title, error) { return board.History(ctx, tx, 1) })
	if err != nil || len(hist) != 2 || hist[0].Title != "Final" || hist[1].Version != 2 || !hist[1].At.Equal(now) {
		t.Errorf("history %+v, %v", hist, err)
	}
	if c := load(t, db).Lanes[0].Cards[0]; c.Title != "Shipped" || c.Version != 3 {
		t.Errorf("card %+v", c)
	}
}

// The decision alone, as given, when, then: no database.
func TestDecideRename(t *testing.T) {
	named := eventstest.For(board.Rename).Given(board.Renamed{Title: "Draft"})
	named.When(board.RenameCard{Card: 1, Title: " Final "}).Then(t, board.Renamed{Title: "Final"})
	named.When(board.RenameCard{Card: 1, Title: "Draft"}).Then(t)
	named.When(board.RenameCard{Card: 1, Title: " "}).ThenError(t, &writer.Refusal{Msg: "A title has 1 to 200 characters."})
}

// Every event type keeps a sample in testdata, as it was first stored: a
// field renamed instead of upcast fails here.
func TestEventSamples(t *testing.T) {
	eventstest.Samples(t, board.Events, board.Cards, os.DirFS("testdata/card"))
}

// TestRunner runs the board's event store as the app does, a Runner
// beside the writer. A database that an older version of the app left,
// with card_history at version 0, gets the table rebuilt from the cards'
// streams while the board goes on.
func TestRunner(t *testing.T) {
	db, w, _ := open(t)
	ctx := context.Background()
	for _, title := range []string{"a", "b"} {
		if err := w.Do(ctx, board.AddCard{Lane: 1, Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []board.RenameCard{{Card: 1, Title: "A"}, {Card: 2, Title: "B"}, {Card: 1, Title: "AA"}} {
		if err := w.Do(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	old := writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(`UPDATE lith_projections SET version = 0 WHERE name = 'card_history'; DELETE FROM card_history`)
		return err
	})
	if err := w.Do(ctx, old); err != nil {
		t.Fatal(err)
	}
	w.Close() // the old app stops; the new one starts
	w, err := writer.New(db, writer.Options{})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(ctx)
	defer w.Close()

	runCtx, stop := context.WithCancel(ctx)
	runner := events.NewRunner(board.Events, db, w, events.Options{})
	done := make(chan error, 1)
	go func() { done <- runner.Run(runCtx) }()
	defer func() {
		stop() // before the writer closes
		<-done
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.ContainsFunc(runner.Stats().Projectors, func(p events.ProjectorStats) bool {
		return p.Name == "card_history" && p.Status == "live"
	}) {
		if time.Now().After(deadline) {
			t.Fatalf("no rebuild: %+v", runner.Stats())
		}
		time.Sleep(time.Millisecond)
	}
	hist, err := db.ViewValue(ctx, func(tx *sql.Tx) ([]board.Title, error) { return board.History(ctx, tx, 1) })
	if err != nil || len(hist) != 2 || hist[1].Title != "AA" {
		t.Errorf("history after the rebuild: %+v, %v", hist, err)
	}
}

func TestOutbox(t *testing.T) {
	db, w, _ := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sent := make(chan string, 1)
	out := board.NewOutbox(db, w, func(_ context.Context, to, subject string) error {
		sent <- to + ": " + subject
		return nil
	}, nil)
	go out.Run(ctx)
	if err := w.Do(ctx, board.AddCard{Lane: 1, Title: "Docs"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Do(ctx, board.Assign{Card: 1, Email: "ada@example.com", At: now, Wake: out.Wake}); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-sent:
		if got != "ada@example.com: A card for you: Docs" {
			t.Errorf("sent %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no mail after the commit")
	}
}

// A count holds the assignments before it and none after, also in one
// batch with them; an archive publishes nothing, and the next count takes
// the card off.
func TestWorkload(t *testing.T) {
	_, w, _ := open(t)
	ctx, cancel := context.WithCancel(context.Background())
	for _, title := range []string{"a", "b", "c"} {
		if err := w.Do(ctx, board.AddCard{Lane: 1, Title: title}); err != nil {
			t.Fatal(err)
		}
	}
	assign := func(card int64, who string) board.Assign {
		return board.Assign{Card: card, Email: who + "@example.com", At: now}
	}
	if err := w.Do(ctx, assign(1, "ada")); err != nil { // before the first count
		t.Fatal(err)
	}
	var l board.Workload
	done := make(chan struct{})
	go func() {
		l.Run(ctx, w, time.Hour, slog.Default()) // no count but the first and the test's
		close(done)
	}()
	defer func() {
		cancel()
		<-done
	}()
	wait := func(ada, bob int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for l.Open("ada@example.com") != ada || l.Open("bob@example.com") != bob {
			if time.Now().After(deadline) {
				t.Fatalf("ada %d, bob %d, want %d and %d", l.Open("ada@example.com"), l.Open("bob@example.com"), ada, bob)
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait(1, 0)

	// One batch: an assignment, a count, an assignment.
	started, gate := make(chan struct{}), make(chan struct{})
	go w.Do(ctx, writer.Func(func(*writer.Tx) error { close(started); <-gate; return nil }))
	<-started
	for _, cmd := range []writer.Command{assign(2, "ada"), board.CountOpen{}, assign(3, "bob")} {
		if err := w.Send(cmd); err != nil {
			t.Fatal(err)
		}
	}
	close(gate)
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	wait(2, 1)

	if err := w.Do(ctx, board.ArchiveCards{Board: 1, Cards: []int64{1}, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := w.Do(ctx, board.CountOpen{}); err != nil {
		t.Fatal(err)
	}
	wait(1, 1)
}

func TestImport(t *testing.T) {
	db, w, touched := open(t)
	ctx := context.Background()
	var cards []board.Card
	for i := 5000; i > 0; i-- {
		cards = append(cards, board.Card{ID: int64(i), Lane: 1, Title: "imported"})
	}
	if err := board.Import(ctx, w, cards); err != nil {
		t.Fatal(err)
	}
	if got := touched(); !slices.Equal(got, []string{"board:1"}) {
		t.Errorf("touched %v", got)
	}
	if b := load(t, db); len(b.Lanes[0].Cards) != 5000 || b.Lanes[0].Cards[0].ID != 1 {
		t.Errorf("imported %d cards", len(b.Lanes[0].Cards))
	}
}

func TestImportFile(t *testing.T) {
	db, w, _ := open(t)
	ctx := context.Background()
	src, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "other.db"), sqlite.Options{Migrations: migrations.FS})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.W.ExecContext(ctx, `INSERT INTO boards VALUES (7, 'Other');
		INSERT INTO lanes VALUES (70, 7, 'Lane', 50);
		INSERT INTO cards (id, lane_id, title) VALUES (700, 70, 'From afar')`); err != nil {
		t.Fatal(err)
	}
	path := src.Path
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}
	if err := board.ImportFile(ctx, w, path); err != nil {
		t.Fatal(err)
	}
	// Commands run as before, on statements prepared again.
	if err := w.Do(ctx, board.AddCard{Lane: 70, Title: "Here"}); err != nil {
		t.Fatal(err)
	}
	b, err := db.ViewValue(ctx, func(tx *sql.Tx) (board.Board, error) { return board.Load(ctx, tx, 7) })
	if err != nil || len(b.Lanes) != 1 || len(b.Lanes[0].Cards) != 2 {
		t.Errorf("imported board %+v, %v", b, err)
	}
}

func TestPrefsAndPanel(t *testing.T) {
	db, w, touched := open(t)
	ctx := context.Background()
	if err := w.Do(ctx, board.SetCompact{Session: "s1", Compact: true, At: now}); err != nil {
		t.Fatal(err)
	}
	if err := w.Do(ctx, board.OpenPanel{Session: "s1", Tab: "t1", Card: 3, At: now}); err != nil {
		t.Fatal(err)
	}
	if got := touched(); !slices.Equal(got, []string{"sid:s1", "tab:t1"}) {
		t.Errorf("touched %v", got)
	}
	p, err := db.ViewValue(ctx, func(tx *sql.Tx) (board.Prefs, error) { return board.SessionPrefs(ctx, tx, "s1") })
	if err != nil || !p.Compact {
		t.Errorf("prefs %+v %v", p, err)
	}
	s, err := db.ViewValue(ctx, func(tx *sql.Tx) (board.TabState, error) { return board.Tab(ctx, tx, "s1", "t1") })
	if err != nil || s.Panel != 3 {
		t.Errorf("tab %+v %v", s, err)
	}
}

// TestFlush sends without waiting, then reads after Flush: commands apply
// in the order they arrive, so the read sees all of them.
func TestFlush(t *testing.T) {
	db, w, _ := open(t)
	ctx := context.Background()
	for range 3 {
		if err := w.Send(board.AddCard{Lane: 1, Title: "sent"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(load(t, db).Lanes[0].Cards); n != 3 {
		t.Errorf("%d cards after Flush", n)
	}
}

// TestStore reads through a Store with the ctx of a GET's transaction: the
// store joins it, and sees what the transaction sees.
func TestStore(t *testing.T) {
	db, w, _ := open(t)
	ctx := context.Background()
	if _, err := board.Create(ctx, w, ""); !errors.Is(err, writer.ErrInvalid) && err == nil {
		t.Error("an empty name was accepted")
	}
	id, err := board.Create(ctx, w, "Second")
	if err != nil || id != 2 {
		t.Fatalf("Create: %d %v", id, err)
	}
	store := board.Store{DB: db}
	err = db.View(ctx, func(tx *sql.Tx) error {
		if _, err := board.Load(ctx, tx, id); err != nil { // the snapshot begins with the first read
			return err
		}
		if err := w.Do(context.Background(), board.AddCard{Lane: 4, Title: "After the snapshot"}); err != nil {
			return err
		}
		ctx := sqlite.WithTx(ctx, tx) // code that takes only a ctx joins this snapshot
		b, err := store.Board(ctx, id)
		if err == nil && len(b.Lanes[1].Cards) != 0 {
			t.Errorf("the store read outside the snapshot: %+v", b.Lanes[1])
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if b, err := store.Board(ctx, id); err != nil || len(b.Lanes[1].Cards) != 1 {
		t.Errorf("a new snapshot: %+v %v", b, err)
	}
}

func TestCheck(t *testing.T) {
	db, _, _ := open(t)
	if err := board.Check(context.Background(), db.Path); err != nil {
		t.Error(err)
	}
	if err := board.Check(context.Background(), filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Error("a missing copy passed")
	}
}
