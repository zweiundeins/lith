package edge

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Health answers 200 "ok <version>" while the process serves. It checks
// nothing, so a busy database never fails it. Mount it at /healthz.
func Health(version string) http.Handler {
	body := []byte("ok " + cmp.Or(version, "dev") + "\n")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "text/plain; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		w.Write(body)
	})
}

// Ready answers 200 when every check passes within Timeout, and 503 naming
// those that did not, so the proxy takes the instance out of rotation. The
// checks run in parallel, with a context that ends at the timeout, and the
// probe answers by then even if a check ignores it.
//
// A check must not queue behind the writer: a command waits for every batch
// ahead of it, which turns a busy writer into a failed probe. Read from the
// read pool (SELECT 1), or look at state in memory.
type Ready struct {
	// Timeout bounds all checks together, 2 seconds when zero.
	Timeout time.Duration
	// Log gets each failure with its error, slog.Default() when nil.
	Log *slog.Logger

	mu     sync.Mutex
	checks []readyCheck
}

type readyCheck struct {
	name  string
	check func(context.Context) error
}

// Add registers a check under a name for the 503 and the log.
func (rd *Ready) Add(name string, check func(context.Context) error) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	rd.checks = append(rd.checks, readyCheck{name, check})
}

func (rd *Ready) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rd.mu.Lock()
	checks := slices.Clone(rd.checks)
	rd.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), cmp.Or(rd.Timeout, 2*time.Second))
	defer cancel()

	type result struct {
		i   int
		err error
	}
	results := make(chan result, len(checks))
	for i, c := range checks {
		go func() {
			defer func() {
				if v := recover(); v != nil {
					results <- result{i, fmt.Errorf("panic: %v", v)}
				}
			}()
			results <- result{i, c.check(ctx)}
		}()
	}
	errs := make([]error, len(checks))
	for i := range errs {
		errs[i] = context.DeadlineExceeded
	}
wait:
	for range checks {
		select {
		case res := <-results:
			errs[res.i] = res.err
		case <-ctx.Done():
			break wait
		}
	}

	var failed []string
	log := cmp.Or(rd.Log, slog.Default())
	for i, err := range errs {
		if err != nil {
			failed = append(failed, checks[i].name)
			log.LogAttrs(r.Context(), slog.LevelWarn, "not ready", slog.String("check", checks[i].name), slog.Any("err", err))
		}
	}
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	if failed != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, "not ready: %s\n", strings.Join(failed, ", "))
		return
	}
	w.Write([]byte("ready\n"))
}
