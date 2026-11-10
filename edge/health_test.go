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

// Ready logs a check once when it starts to fail, at Info until a probe
// has passed, as while the static files compress at startup, and at Warn
// after; and once when the probe passes, the first time or again.
func TestReadyLogsOncePerSpell(t *testing.T) {
	var logs bytes.Buffer
	rd := &Ready{Log: slog.New(slog.NewTextHandler(&logs, nil))}
	var static, database error
	rd.Add("database", func(context.Context) error { return database })
	rd.Add("static", func(context.Context) error { return static })
	step := func(name string, probes int, want ...string) {
		t.Helper()
		logs.Reset()
		for range probes {
			rd.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/readyz", nil))
		}
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		if logs.Len() == 0 {
			lines = nil
		}
		ok := len(lines) == len(want)
		for i := 0; ok && i < len(want); i++ {
			ok = strings.Contains(lines[i], want[i])
		}
		if !ok {
			t.Errorf("%s: logged\n%s\nwant one line each with %q", name, logs.String(), want)
		}
	}

	static = errors.New("compressing the static files")
	step("startup", 5, `level=INFO msg="not ready" check=static err="compressing the static files"`)
	static = nil
	step("compressed", 3, "level=INFO msg=ready after=")
	database = errors.New("disk I/O error")
	step("database down", 4, `level=WARN msg="not ready" check=database err="disk I/O error"`)
	static = errors.New("compressing the static files")
	step("static too", 2, `level=WARN msg="not ready" check=static`)
	database = nil
	step("database back", 2)
	static = nil
	step("all back", 2, "level=INFO msg=ready after=")

	first := &Ready{Log: rd.Log}
	first.Add("database", func(context.Context) error { return nil })
	rd = first
	step("ready at once", 3, "level=INFO msg=ready")
	if strings.Contains(logs.String(), "after=") {
		t.Errorf("a first pass names a spell: %s", logs.String())
	}
}
