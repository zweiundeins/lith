package web

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestShared: the tabs that refresh at once share one render, and so do
// those that come within sharedFor after it; a later one renders again,
// and a render older than sharedFor is dropped from the map. A
// failed render is not shared: a caller that waited for it renders itself.
func TestShared(t *testing.T) {
	var s shared
	var renders atomic.Int32
	render := func() ([]byte, error) {
		n := renders.Add(1)
		time.Sleep(50 * time.Millisecond)
		return []byte{byte(n)}, nil
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			ev, err := s.do(context.Background(), "/", render)
			if err != nil || len(ev) != 1 || ev[0] != 1 {
				t.Errorf("got %v, %v", ev, err)
			}
		})
	}
	wg.Wait()
	if n := renders.Load(); n != 1 {
		t.Fatalf("%d renders for 20 tabs at once", n)
	}
	if ev, _ := s.do(context.Background(), "/", render); ev[0] != 1 || renders.Load() != 1 {
		t.Fatalf("a tab right after rendered again: %v", ev)
	}
	if ev, _ := s.do(context.Background(), "/other", render); ev[0] != 2 {
		t.Fatalf("another page shared the render: %v", ev)
	}
	time.Sleep(sharedFor)
	if ev, _ := s.do(context.Background(), "/", render); ev[0] != 3 {
		t.Fatalf("a tab after sharedFor got the old render: %v", ev)
	}
	s.mu.Lock()
	_, kept := s.calls["/other"]
	s.mu.Unlock()
	if kept {
		t.Fatal("a render older than sharedFor was kept: keys by session would grow without bound")
	}

	fail := errors.New("the database is gone")
	started := make(chan struct{})
	go s.do(context.Background(), "/failing", func() ([]byte, error) {
		close(started)
		time.Sleep(50 * time.Millisecond)
		return nil, fail
	})
	<-started
	ev, err := s.do(context.Background(), "/failing", func() ([]byte, error) { return []byte("own"), nil })
	if err != nil || string(ev) != "own" {
		t.Fatalf("after a failed render: %q, %v", ev, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	started = make(chan struct{})
	go s.do(context.Background(), "/slow", func() ([]byte, error) { close(started); <-release; return nil, nil })
	<-started
	cancel()
	if _, err := s.do(ctx, "/slow", render); !errors.Is(err, context.Canceled) {
		t.Fatalf("a caller that left: %v", err)
	}
	close(release)
}
