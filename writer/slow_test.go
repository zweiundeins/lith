package writer

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/sqlite"
)

// slowLog collects the writer's log.
type slowLog struct {
	mu   sync.Mutex
	msgs []string
}

func (l *slowLog) Enabled(context.Context, slog.Level) bool { return true }
func (l *slowLog) WithAttrs([]slog.Attr) slog.Handler       { return l }
func (l *slowLog) WithGroup(string) slog.Handler            { return l }
func (l *slowLog) Handle(_ context.Context, r slog.Record) error {
	s := r.Message
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == "cmd" {
			s += " " + a.Value.String()
		}
		return true
	})
	l.mu.Lock()
	l.msgs = append(l.msgs, s)
	l.mu.Unlock()
	return nil
}

func (l *slowLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.msgs)
}

type stuck struct{ gate chan struct{} }

func (s stuck) Apply(*Tx) error { <-s.gate; return nil }

func TestSlowCommand(t *testing.T) {
	log := &slowLog{}
	f := start(t, sqlite.Options{}, Options{SlowCommand: 50 * time.Millisecond, Log: slog.New(log)})
	ctx := context.Background()
	if err := f.w.Do(ctx, increment(1)); err != nil {
		t.Fatal(err)
	}
	if r := f.w.Stats().Running; r != 0 {
		t.Fatalf("Running %v between commands", r)
	}
	cmd := stuck{make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- f.w.Do(ctx, cmd) }()
	waitFor(t, "the slow command's log line", func() bool { return len(log.get()) == 1 })
	if r := f.w.Stats().Running; r < 50*time.Millisecond {
		t.Errorf("Running %v while the command waits", r)
	}
	time.Sleep(100 * time.Millisecond)
	if msgs := log.get(); len(msgs) != 1 || !strings.Contains(msgs[0], "writer.stuck") {
		t.Fatalf("logged %q, want one line with the command's type", msgs)
	}
	close(cmd.gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if msgs := log.get(); len(msgs) != 2 || !strings.Contains(msgs[1], "ended") {
		t.Fatalf("logged %q, want the end too", msgs)
	}
	if r := f.w.Stats().Running; r != 0 {
		t.Fatalf("Running %v after the command", r)
	}
	if err := f.w.Do(ctx, increment(1)); err != nil {
		t.Fatal(err)
	}
	if n := len(log.get()); n != 2 {
		t.Fatalf("a quick command was logged: %q", log.get())
	}
}

func TestSlowCommandOff(t *testing.T) {
	log := &slowLog{}
	f := start(t, sqlite.Options{}, Options{SlowCommand: -1, Log: slog.New(log)})
	cmd := stuck{make(chan struct{})}
	time.AfterFunc(50*time.Millisecond, func() { close(cmd.gate) })
	if err := f.w.Do(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if msgs := log.get(); len(msgs) != 0 {
		t.Fatalf("logged %q", msgs)
	}
}
