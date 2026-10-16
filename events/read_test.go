package events_test

import (
	"bufio"
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/events"
	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sqlite"
	"github.com/zweiundeins/lith/sse"
	"github.com/zweiundeins/lith/writer"
)

// Load and Since read through a live render's Querier, on the worker's
// statements in the render's snapshot; a loop that breaks off frees its
// statement for the next query. The feed renders again when a command
// appends, through events.Topic.
func TestReadsInARender(t *testing.T) {
	f := open(t, titles(1, same))
	f.createMany(t, 1, 3)
	hub := live.New(live.Options{Read: f.db.R, Tick: time.Millisecond, Log: slog.New(slog.DiscardHandler)})
	f.w.Close() // a writer that tells the hub
	w, err := writer.New(f.db, writer.Options{OnCommit: hub.Changed, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	go w.Run(context.Background())
	t.Cleanup(func() { w.Close() })
	f.w = w
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)

	feed := live.View{Key: "feed", Topics: []string{events.Topic}, Render: func(ctx context.Context, tx *sql.Tx) (any, error) {
		q, ok := sqlite.QuerierFrom(ctx)
		if !ok {
			return nil, fmt.Errorf("no Querier")
		}
		var b strings.Builder
		for raw, err := range events.Since(ctx, q, 0) {
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&b, "%s %v; ", raw.Type, raw.ID)
			break
		}
		for r, err := range events.Load(ctx, f.st, q, Cards.ID(3), 0) {
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&b, "card 3 %s; ", r.Type)
		}
		n := 0
		for _, err := range events.Since(ctx, q, 0) {
			if err != nil {
				return nil, err
			}
			n++
		}
		fmt.Fprintf(&b, "%d events", n)
		return sse.PatchElements([]byte(`<p id="feed">` + b.String() + `</p>`)), nil
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.Serve(w, r, live.Spec{Views: []live.View{feed}})
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	lines := make(chan string, 64)
	go func() {
		sc := bufio.NewScanner(res.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	next := func(want string) {
		t.Helper()
		timeout := time.After(10 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("the stream ended before %q", want)
				}
				if strings.Contains(l, want) {
					return
				}
			case <-timeout:
				t.Fatalf("no %q", want)
			}
		}
	}
	next("card.created 1; card 3 card.created; 3 events")
	f.rename(t, 3, "renamed")
	next("card.created 1; card 3 card.created; card 3 card.renamed; 4 events")
}

// A GET's read transaction is one snapshot: events appended while it
// reads are not in it.
func TestReadsInASnapshot(t *testing.T) {
	f := open(t, titles(1, same))
	f.createMany(t, 1, 5)
	ctx := context.Background()
	err := f.db.View(ctx, func(tx *sql.Tx) error {
		n := 0
		for _, err := range events.Since(ctx, tx, 0) {
			if err != nil {
				return err
			}
			if n++; n == 2 {
				f.createMany(t, 6, 10)
			}
		}
		if n != 5 {
			t.Errorf("read %d events", n)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM lith_events`); n != 10 {
		t.Errorf("%d events", n)
	}
}
