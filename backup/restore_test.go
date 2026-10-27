package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// connCounter is an S3 endpoint that counts the client's connections, open
// and at most at once, and its GETs of LTX files. onGet, if set, answers
// the n-th of them (from 1) in place of the bucket when it returns true.
type connCounter struct {
	srv   *httptest.Server
	open  atomic.Int64
	peak  atomic.Int64
	gets  atomic.Int64
	onGet atomic.Pointer[func(n int64, w http.ResponseWriter) bool]
}

func newConnCounter(t *testing.T) *connCounter {
	t.Helper()
	c := &connCounter{}
	next := fakeS3(t)
	c.srv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, ".ltx") {
			n := c.gets.Add(1)
			if f := c.onGet.Load(); f != nil && (*f)(n, w) {
				return
			}
		}
		next.ServeHTTP(w, r)
	}))
	c.srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			n := c.open.Add(1)
			for p := c.peak.Load(); n > p && !c.peak.CompareAndSwap(p, n); p = c.peak.Load() {
			}
		case http.StateClosed, http.StateHijacked:
			c.open.Add(-1)
		}
	}
	c.srv.Start()
	t.Cleanup(c.srv.Close)
	return c
}

// manySmallFiles replicates a database to the counter's bucket as a snapshot
// and then files WAL segments of one row each, as a replica under constant
// writes gathers them between two snapshots.
func manySmallFiles(t *testing.T, c *connCounter, files int) *Manager {
	t.Helper()
	m, err := New(Config{
		Bucket: "lith", Endpoint: c.srv.URL, Region: "us-east-1", ForcePathStyle: true,
		AccessKeyID: "test", SecretAccessKey: "test", Logger: slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app.db")
	db, detach := replicated(t, m, "app", path)
	m.mu.Lock()
	ldb := m.attached["app"].db
	m.mu.Unlock()
	for i := range files {
		mustExec(t, db, `INSERT INTO notes (body) VALUES (?)`, fmt.Sprintf("row %d", i))
		if err := ldb.SyncAndWait(bg); err != nil {
			t.Fatal(err)
		}
	}
	shutdown(t, db, detach)
	if n := countLevel(t, m, "app", 0); n < files {
		t.Fatalf("level 0 has %d files, want at least %d", n, files)
	}
	return m
}

// settle closes the connections the client keeps idle and waits for the
// goroutines to fall back to at most base, which a leak never does.
func settle(t *testing.T, c *connCounter, base int) {
	t.Helper()
	c.srv.CloseClientConnections()
	deadline := time.Now().Add(10 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<20)
			t.Fatalf("goroutines = %d after the restore, %d before\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRestoreBoundsItsConnections restores a replica of several hundred
// small files: passed, cancelled half-way and failed half-way. A restore
// holds a few connections at a time and leaves no goroutine behind.
func TestRestoreBoundsItsConnections(t *testing.T) {
	const files = 300
	c := newConnCounter(t)
	m := manySmallFiles(t, c, files)

	for _, tc := range []struct {
		name  string
		onGet func(cancel context.CancelFunc) func(int64, http.ResponseWriter) bool
		want  func(error) bool
	}{
		{"passed", nil, func(err error) bool { return err == nil }},
		{"cancelled", func(cancel context.CancelFunc) func(int64, http.ResponseWriter) bool {
			return func(n int64, _ http.ResponseWriter) bool {
				if n == files/2 {
					cancel()
				}
				return false
			}
		}, func(err error) bool { return errors.Is(err, context.Canceled) }},
		{"failed", func(context.CancelFunc) func(int64, http.ResponseWriter) bool {
			return func(n int64, w http.ResponseWriter) bool {
				if n != files/2 {
					return false
				}
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>AccessDenied</Code><Message>no</Message></Error>`))
				return true
			}
		}, func(err error) bool { return err != nil && !errors.Is(err, context.Canceled) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c.srv.CloseClientConnections()
			time.Sleep(50 * time.Millisecond)
			base := runtime.NumGoroutine()
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			c.onGet.Store(nil)
			if tc.onGet != nil {
				f := tc.onGet(cancel)
				c.onGet.Store(&f)
			}
			c.gets.Store(0)
			c.peak.Store(c.open.Load())

			var peakG atomic.Int64
			done := make(chan struct{})
			var wg sync.WaitGroup
			wg.Go(func() {
				for {
					if n := int64(runtime.NumGoroutine()); n > peakG.Load() {
						peakG.Store(n)
					}
					select {
					case <-done:
						return
					case <-time.After(time.Millisecond):
					}
				}
			})
			err := m.VerifyRestore(ctx, "app")
			close(done)
			wg.Wait()
			c.onGet.Store(nil)
			t.Logf("%s: error %v; %d LTX GETs, peak %d connections, peak %d goroutines over %d", tc.name, err, c.gets.Load(), c.peak.Load(), peakG.Load(), base)
			if !tc.want(err) {
				t.Errorf("VerifyRestore = %v", err)
			}
			if p := c.peak.Load(); p > restoreFetchers+2 {
				t.Errorf("peak connections = %d, want at most %d", p, restoreFetchers+2)
			}
			settle(t, c, base)
		})
	}
	out := filepath.Join(t.TempDir(), "restored.db")
	if err := m.Restore(bg, "app", out); err != nil {
		t.Fatal(err)
	}
	if got := bodies(t, out); len(got) != files+1 || got[files] != fmt.Sprintf("row %d", files-1) {
		t.Errorf("restored %d rows, want %d", len(got), files+1)
	}
}
