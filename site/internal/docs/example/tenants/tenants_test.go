package tenants_test

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/writer"

	"github.com/zweiundeins/lith/site/internal/docs/example/board"
	"github.com/zweiundeins/lith/site/internal/docs/example/tenants"
)

func TestAccounts(t *testing.T) {
	dir := t.TempDir()
	hub := live.New(live.Options{Tick: 5 * time.Millisecond}) // no Read: every view reads its account's file
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)
	accts := tenants.Open(dir, 1, hub, nil)
	defer accts.Close()

	for _, key := range []string{"acme", "globex"} {
		if err := accts.Do(ctx, key, writer.OnceResult("seed", board.CreateBoard{Name: key})); err != nil {
			t.Fatal(err)
		}
		if err := accts.Do(ctx, key, board.AddCard{Lane: 1, Title: "First"}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, key+".db")); err != nil {
			t.Errorf("%s has no file of its own: %v", key, err)
		}
	}
	if s := accts.Stats(); s.Open != 1 || s.Evictions == 0 {
		t.Errorf("a cap of one: %+v", s)
	}
	if got := tenants.Topic("acme", board.Topic(1)); got != "acct:acme:board:1" {
		t.Errorf("topic %q", got)
	}
	if err := accts.Do(ctx, "../etc", board.AddCard{Lane: 1, Title: "x"}); err == nil {
		t.Error("a key that is no file name was accepted")
	}

	if err := accts.Erase(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "acme.db")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("acme's file after Erase: %v", err)
	}
	if err := accts.Do(ctx, "acme", board.AddCard{Lane: 1, Title: "Back"}); !errors.Is(err, tenants.ErrErased) {
		t.Errorf("an erased account opened again: %v", err)
	}
}

// TestCountView renders an account's view on a hub that has no read pool:
// the render reads from the account's own file.
func TestCountView(t *testing.T) {
	hub := live.New(live.Options{Tick: 5 * time.Millisecond})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.Run(ctx)
	accts := tenants.Open(t.TempDir(), 4, hub, nil)
	defer accts.Close()
	if err := accts.Do(ctx, "acme", writer.OnceResult("seed", board.CreateBoard{Name: "Acme"})); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hub.Serve(w, r, live.Spec{Views: []live.View{accts.CountView("acme", 1)}})
	}))
	defer srv.Close()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	req.Header.Set("Accept-Encoding", "identity")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	lines := bufio.NewScanner(res.Body)
	want := []string{"Cards on the board: 0", "Cards on the board: 1"}
	for lines.Scan() && len(want) > 0 {
		if strings.Contains(lines.Text(), want[0]) {
			want = want[1:]
			if len(want) == 1 { // the first frame arrived: add a card
				if err := accts.Do(ctx, "acme", board.AddCard{Lane: 1, Title: "New"}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if len(want) > 0 {
		t.Errorf("the stream ended before %q", want[0])
	}
}
