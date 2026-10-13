// Package apptest runs the whole site in process for tests: a database of
// its own, the edge, the writer, the hub, on a port of 127.0.0.1.
package apptest

import (
	"bytes"
	"context"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/site/internal/app"
)

// Start runs the site until the test ends and returns it with its base URL.
// change, if given, adjusts the configuration first. The site's log goes
// to the test's output if the test fails.
func Start(t testing.TB, change ...func(*app.Config)) (*app.App, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + ln.Addr().String()
	logs := &buffer{}
	cfg := app.Config{
		DB:      filepath.Join(t.TempDir(), "site.db"),
		Base:    base,
		Repo:    "https://github.com/zweiundeins/lith",
		Version: "test",
		Log:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	for _, c := range change {
		c(&cfg)
	}
	a, err := app.New(context.Background(), cfg)
	if err != nil {
		ln.Close()
		t.Fatal(err)
	}
	a.Start()
	srv := &http.Server{Handler: a.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(ln)
	t.Cleanup(func() {
		a.StopStreams()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		if err := a.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
		if t.Failed() {
			t.Logf("the site's log:\n%s", logs.String())
		}
	})
	return a, base
}

type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
