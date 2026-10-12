package edge

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	for version, want := range map[string]string{"v1.4.2": "ok v1.4.2\n", "": "ok dev\n"} {
		w := httptest.NewRecorder()
		Health(version).ServeHTTP(w, httptest.NewRequest("GET", "/healthz", nil))
		if w.Code != 200 || w.Body.String() != want || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("Health(%q): %d %q %v", version, w.Code, w.Body, w.Header())
		}
	}
}

func TestReady(t *testing.T) {
	var logs bytes.Buffer
	rd := &Ready{Timeout: 100 * time.Millisecond, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	probe := func() (int, string, time.Duration) {
		start := time.Now()
		w := httptest.NewRecorder()
		rd.ServeHTTP(w, httptest.NewRequest("GET", "/readyz", nil))
		return w.Code, w.Body.String(), time.Since(start)
	}

	if code, body, _ := probe(); code != 200 || body != "ready\n" {
		t.Errorf("no checks: %d %q", code, body)
	}
	rd.Add("db", func(ctx context.Context) error { return nil })
	if code, body, _ := probe(); code != 200 || body != "ready\n" {
		t.Errorf("passing check: %d %q", code, body)
	}

	rd.Add("disk", func(ctx context.Context) error { return errors.New("disk full") })
	rd.Add("waits", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	rd.Add("stuck", func(ctx context.Context) error { time.Sleep(time.Second); return nil })
	rd.Add("panics", func(ctx context.Context) error { panic("boom") })
	code, body, took := probe()
	if code != 503 || body != "not ready: disk, waits, stuck, panics\n" {
		t.Errorf("failing checks: %d %q", code, body)
	}
	if took > 500*time.Millisecond {
		t.Errorf("the probe took %v with a timeout of 100ms", took)
	}
	for _, want := range []string{"check=disk", "disk full", "check=stuck", "deadline exceeded", "check=panics", "panic: boom"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log lacks %q:\n%s", want, logs.String())
		}
	}
	if strings.Contains(logs.String(), "check=db") {
		t.Error("a passing check was logged")
	}
}
