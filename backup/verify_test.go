package backup

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	dto "github.com/prometheus/client_model/go"
)

var quiet = slog.New(slog.DiscardHandler)

// fakeVerifier fails each replica for its first fails[name] attempts. It
// records when each verification ran and can take time doing it.
type fakeVerifier struct {
	names []string
	fails map[string]int
	took  time.Duration // each VerifyAll

	mu       sync.Mutex
	attempts map[string]int
	alls     []time.Time
	retries  []time.Time
	onVerify func(name string, attempt int)
}

func newFakeVerifier(fails map[string]int, names ...string) *fakeVerifier {
	return &fakeVerifier{names: names, fails: fails, attempts: map[string]int{}}
}

func (f *fakeVerifier) Enabled() bool { return true }

func (f *fakeVerifier) VerifyAll(ctx context.Context) map[string]error {
	f.mu.Lock()
	f.alls = append(f.alls, time.Now())
	f.mu.Unlock()
	select {
	case <-ctx.Done():
	case <-time.After(f.took):
	}
	out := map[string]error{}
	for _, n := range f.names {
		out[n] = f.verify(ctx, n)
	}
	return out
}

func (f *fakeVerifier) VerifyRestore(ctx context.Context, name string) error {
	f.mu.Lock()
	f.retries = append(f.retries, time.Now())
	f.mu.Unlock()
	return f.verify(ctx, name)
}

func (f *fakeVerifier) verify(ctx context.Context, name string) error {
	f.mu.Lock()
	f.attempts[name]++
	n := f.attempts[name]
	cb := f.onVerify
	f.mu.Unlock()
	if cb != nil {
		cb(name, n)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if n <= f.fails[name] {
		return fmt.Errorf("attempt %d: 503 SlowDown", n)
	}
	return nil
}

var shortRetries = []time.Duration{time.Millisecond, time.Millisecond}

// verifyOnce runs one run with the retries, the next run a day away.
func verifyOnce(ctx context.Context, v verifier, retries []time.Duration) (verifyRun, bool) {
	now := time.Now()
	return verifyWithRetries(ctx, v, quiet, retries, now, now.Add(24*time.Hour))
}

func TestVerifyRetriesOnlyTheFailedReplicas(t *testing.T) {
	v := newFakeVerifier(map[string]int{"a": 1, "b": 2}, "a", "b", "c")
	run, ok := verifyOnce(t.Context(), v, shortRetries)
	if !ok || len(run.failures) != 0 {
		t.Fatalf("ok=%v failures=%v, want none", ok, run.failures)
	}
	if v.attempts["a"] != 2 || v.attempts["b"] != 3 || v.attempts["c"] != 1 {
		t.Fatalf("attempts = %v, want a:2 b:3 c:1", v.attempts)
	}
	if !slices.Equal(run.recovered, []string{"a", "b"}) {
		t.Errorf("recovered = %v, want [a b]", run.recovered)
	}
	if want := map[string]int{"a": 2, "b": 3, "c": 1}; !maps.Equal(run.attempts, want) {
		t.Errorf("run attempts = %v, want %v", run.attempts, want)
	}
}

func TestVerifyPersistentFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		v := newFakeVerifier(map[string]int{"a": 99, "b": 99}, "a", "b", "c")
		run, ok := verifyOnce(t.Context(), v, []time.Duration{10 * time.Minute, 20 * time.Minute})
		if !ok || len(run.failures) != 2 {
			t.Fatalf("ok=%v failures=%v, want a and b", ok, run.failures)
		}
		for i, name := range []string{"a", "b"} {
			f := run.failures[i]
			if f.Name != name || f.Attempts != 3 {
				t.Errorf("failure %d = %+v, want %s after 3 attempts", i, f, name)
			}
			if f.First == nil || f.Last == nil || f.First.Error() != "attempt 1: 503 SlowDown" || f.Last.Error() != "attempt 3: 503 SlowDown" {
				t.Errorf("%s: first %v, last %v; want attempts 1 and 3", name, f.First, f.Last)
			}
		}
		if run.span != 30*time.Minute {
			t.Errorf("span = %v, want 30m", run.span)
		}
		if len(run.recovered) != 0 {
			t.Errorf("recovered = %v, want none", run.recovered)
		}
	})
}

func TestVerifyMixedRun(t *testing.T) {
	v := newFakeVerifier(map[string]int{"a": 1, "b": 99}, "a", "b", "c")
	run, ok := verifyOnce(t.Context(), v, shortRetries)
	if !ok || len(run.failures) != 1 || run.failures[0].Name != "b" || run.failures[0].Attempts != 3 {
		t.Fatalf("ok=%v failures=%+v, want only b after 3 attempts", ok, run.failures)
	}
	if v.attempts["a"] != 2 || v.attempts["b"] != 3 || v.attempts["c"] != 1 {
		t.Fatalf("attempts = %v, want a:2 b:3 c:1", v.attempts)
	}
	if !slices.Equal(run.recovered, []string{"a"}) {
		t.Errorf("recovered = %v, want [a]", run.recovered)
	}
}

func TestVerifyWithoutRetries(t *testing.T) {
	v := newFakeVerifier(map[string]int{"a": 1}, "a")
	run, ok := verifyOnce(t.Context(), v, []time.Duration{})
	if !ok || len(run.failures) != 1 || run.failures[0].Attempts != 1 || v.attempts["a"] != 1 {
		t.Fatalf("ok=%v failures=%+v attempts=%v, want one failure, no retry", ok, run.failures, v.attempts)
	}
}

// TestVerifyLoopShutdownDuringARetryWait: a stop during the wait before a
// retry ends the loop at once, without a report.
func TestVerifyLoopShutdownDuringARetryWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		v := newFakeVerifier(map[string]int{"a": 99}, "a")
		recorded := false
		done := make(chan struct{})
		go func() {
			runVerifyLoop(ctx, v, quiet, VerifyOptions{Retries: []time.Duration{time.Hour}},
				func(verifyRun) { recorded = true })
			close(done)
		}()
		time.Sleep(10*time.Minute + 30*time.Minute)
		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("the loop did not stop during the retry wait")
		}
		if recorded || v.attempts["a"] != 1 {
			t.Fatalf("recorded=%v attempts=%v, want no report and no retry", recorded, v.attempts)
		}
	})
}

// TestVerifyLoopShutdownDuringARestore: a restore cut short by the stop is
// neither a failure nor reported.
func TestVerifyLoopShutdownDuringARestore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		v := newFakeVerifier(map[string]int{"a": 1}, "a", "b")
		v.onVerify = func(name string, n int) {
			if name == "a" && n == 2 {
				cancel()
			}
		}
		var reports int
		runVerifyLoop(ctx, v, quiet, VerifyOptions{OnFail: func([]VerifyFailure, time.Duration) { reports++ }},
			func(verifyRun) { reports++ })
		if reports != 0 || v.attempts["a"] != 2 {
			t.Fatalf("reports = %d, attempts = %v; want none after a's retry", reports, v.attempts)
		}
	})
}

func TestVerifyLoopCallsOncePerRunWithEveryFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		v := newFakeVerifier(map[string]int{"a": 99, "b": 99}, "a", "b", "c")
		type report struct {
			failures []VerifyFailure
			span     time.Duration
		}
		var reports []report
		go runVerifyLoop(ctx, v, quiet, VerifyOptions{OnFail: func(f []VerifyFailure, span time.Duration) {
			reports = append(reports, report{f, span})
		}}, func(verifyRun) {})
		time.Sleep(10*time.Minute + 24*time.Hour + time.Hour)
		synctest.Wait()
		if len(reports) != 2 {
			t.Fatalf("%d calls in two runs, want 2", len(reports))
		}
		for _, r := range reports {
			if len(r.failures) != 2 || r.failures[0].Name != "a" || r.failures[1].Name != "b" || r.span != 30*time.Minute {
				t.Errorf("call = %+v, want a and b in one call, after 30m", r)
			}
		}
	})
}

func TestVerifyLoopCallsNothingWhenARetryPasses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		v := newFakeVerifier(map[string]int{"a": 1, "b": 2}, "a", "b")
		called := false
		var runs []verifyRun
		go runVerifyLoop(ctx, v, quiet, VerifyOptions{OnFail: func([]VerifyFailure, time.Duration) { called = true }},
			func(r verifyRun) { runs = append(runs, r) })
		time.Sleep(time.Hour)
		synctest.Wait()
		if called {
			t.Fatal("OnFail called although the retries passed")
		}
		if len(runs) != 1 || !slices.Equal(runs[0].recovered, []string{"a", "b"}) {
			t.Fatalf("runs = %+v, want one with a and b recovered", runs)
		}
	})
}

// TestVerifyLoopSchedulesFromTheRunsStart: the next run starts an interval
// after the start of the one before, however long it took, and a retry that
// would start at or after it is skipped.
func TestVerifyLoopSchedulesFromTheRunsStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		t0 := time.Now()
		v := newFakeVerifier(map[string]int{"a": 99}, "a")
		v.took = 5 * time.Minute
		var attempts []int
		go runVerifyLoop(ctx, v, quiet, VerifyOptions{
			Interval: time.Hour,
			Retries:  []time.Duration{10 * time.Minute, 20 * time.Minute, 40 * time.Minute},
			OnFail:   func(f []VerifyFailure, _ time.Duration) { attempts = append(attempts, f[0].Attempts) },
		}, func(verifyRun) {})
		time.Sleep(10*time.Minute + 2*time.Hour + time.Minute)
		synctest.Wait()

		rel := func(ts []time.Time) []time.Duration {
			var out []time.Duration
			for _, x := range ts {
				out = append(out, x.Sub(t0))
			}
			return out
		}
		m := time.Minute
		if got, want := rel(v.alls), []time.Duration{10 * m, 70 * m, 130 * m}; !slices.Equal(got, want) {
			t.Errorf("runs started at %v, want %v", got, want)
		}
		// The third retry would start at 85m, past the next run at 70m.
		if got, want := rel(v.retries), []time.Duration{25 * m, 45 * m, 85 * m, 105 * m}; !slices.Equal(got, want) {
			t.Errorf("retries at %v, want %v", got, want)
		}
		if !slices.Equal(attempts, []int{3, 3}) {
			t.Errorf("attempts per reported run = %v, want [3 3]", attempts)
		}
	})
}

func TestVerifyLoopFirstRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		t0 := time.Now()
		v := newFakeVerifier(nil, "a")
		go runVerifyLoop(ctx, v, quiet, VerifyOptions{Interval: 3 * time.Minute}, func(verifyRun) {})
		time.Sleep(10 * time.Minute)
		synctest.Wait()
		if len(v.alls) != 3 || v.alls[0].Sub(t0) != 3*time.Minute {
			t.Errorf("runs at %v, want the first after one interval when it is under ten minutes", v.alls)
		}
	})
}

// TestVerifyLoopOfAManager: a replica that fails every attempt reaches
// OnFail once, and Status and the metrics count its attempts; one that
// passes on a retry counts as retried.
func TestVerifyLoopOfAManager(t *testing.T) {
	t.Parallel()
	m := newManager(t)
	db, detach := replicated(t, m, "app", filepath.Join(t.TempDir(), "app.db"))
	defer shutdown(t, db, detach)

	m.recordVerifyRun(verifyRun{attempts: map[string]int{"app": 2}, recovered: []string{"app"}})
	mfs := gather(t, m.Collector("app"))
	if v := counter(t, mfs, "app_backup_replica_verify_retried_total", "app"); v != 1 {
		t.Errorf("retried_total = %v, want 1", v)
	}
	if v := gauge(t, mfs, "app_backup_replica_verify_attempts", "app"); v != 2 {
		t.Errorf("verify_attempts = %v, want 2", v)
	}

	err := filepath.WalkDir(m.cfg.Dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(p) != ".ltx" {
			return err
		}
		return os.Truncate(p, 10)
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(bg)
	defer cancel()
	failures := make(chan []VerifyFailure, 1)
	go m.RunVerifyLoop(ctx, VerifyOptions{Interval: 2 * time.Second, Retries: shortRetries, OnFail: func(f []VerifyFailure, _ time.Duration) {
		failures <- f
		cancel()
	}})
	select {
	case f := <-failures:
		if len(f) != 1 || f[0].Name != "app" || f[0].Attempts != 3 || f[0].First == nil {
			t.Fatalf("failures = %+v, want app after 3 attempts", f)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("OnFail never called")
	}
	st := m.Status()[0]
	if st.VerifyOK || st.VerifyAttempts != 3 || st.VerifyRetried != 1 {
		t.Errorf("Status = %+v, want a failed verification after 3 attempts, retried once before", st)
	}
}

func counter(t *testing.T, mfs map[string]*dto.MetricFamily, name, replica string) float64 {
	t.Helper()
	for _, m := range mfs[name].GetMetric() {
		for _, l := range m.GetLabel() {
			if l.GetName() == "replica" && l.GetValue() == replica {
				return m.GetCounter().GetValue()
			}
		}
	}
	t.Fatalf("no %s series for %q", name, replica)
	return 0
}
