package backup

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"time"
)

// defaultVerifyRetries put the retries of a run's failed replicas about 10
// and 30 minutes after it.
var defaultVerifyRetries = []time.Duration{10 * time.Minute, 20 * time.Minute}

// VerifyOptions configure RunVerifyLoop.
type VerifyOptions struct {
	// Interval is the time from one run's start to the next one's; zero means
	// DefaultVerifyInterval.
	Interval time.Duration

	// Retries are the waits before each retry of the replicas still failing,
	// each from the end of the attempt before. Nil means 10 and 20 minutes,
	// about 10 and 30 minutes after the run; an empty slice means none. A retry that would start at or after
	// the next run's start is skipped, with every one after it.
	Retries []time.Duration

	// OnFail, if set, is called once per run, after its retries, with the
	// replicas that failed every attempt, sorted by name, and the run's span:
	// the time from its start to the end of its last attempt. A run in which
	// every replica passed, at once or on a retry, calls nothing.
	OnFail func(failures []VerifyFailure, span time.Duration)
}

// VerifyFailure is a replica that failed every verification of a run. First
// and Last are the errors of its first and last attempt.
type VerifyFailure struct {
	Name        string
	First, Last error
	Attempts    int
}

// verifier is the part of a Manager the verify loop drives.
type verifier interface {
	Enabled() bool
	VerifyAll(ctx context.Context) map[string]error
	VerifyRestore(ctx context.Context, name string) error
}

// verifyRun is the outcome of one run with its retries.
type verifyRun struct {
	attempts  map[string]int
	recovered []string // failed, then passed on a retry
	failures  []VerifyFailure
	span      time.Duration
}

// RunVerifyLoop runs VerifyAll until ctx is done: the first run after at
// most ten minutes, then one every opt.Interval from the start of the one
// before. The replicas a run finds failing are verified again after each of
// opt.Retries, and those that never pass go to opt.OnFail, once per run.
// When ctx ends during a run, the run reports nothing.
func (m *Manager) RunVerifyLoop(ctx context.Context, opt VerifyOptions) {
	runVerifyLoop(ctx, m, m.log, opt, m.recordVerifyRun)
}

func runVerifyLoop(ctx context.Context, v verifier, log *slog.Logger, opt VerifyOptions, record func(verifyRun)) {
	if !v.Enabled() {
		return
	}
	interval := orDefault(opt.Interval, DefaultVerifyInterval)
	retries := opt.Retries
	if retries == nil {
		retries = defaultVerifyRetries
	}
	timer := time.NewTimer(min(10*time.Minute, interval))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		start := time.Now()
		next := start.Add(interval)
		run, ok := verifyWithRetries(ctx, v, log, retries, start, next)
		if !ok {
			return
		}
		record(run)
		if len(run.failures) > 0 && opt.OnFail != nil {
			opt.OnFail(run.failures, run.span)
		}
		timer.Reset(time.Until(next))
	}
}

// verifyWithRetries runs one verification of every replica and retries the
// failed ones while a retry fits before next. ok is false when ctx ended.
func verifyWithRetries(ctx context.Context, v verifier, log *slog.Logger, retries []time.Duration, start, next time.Time) (run verifyRun, ok bool) {
	run.attempts = map[string]int{}
	results := v.VerifyAll(ctx)
	if ctx.Err() != nil {
		return run, false
	}
	first, last := map[string]error{}, map[string]error{}
	for _, name := range slices.Sorted(maps.Keys(results)) {
		run.attempts[name] = 1
		if err := results[name]; err != nil {
			first[name], last[name] = err, err
		} else {
			log.Info("backup: restore verified", "replica", name)
		}
	}
	for i, d := range retries {
		if len(last) == 0 || !time.Now().Add(d).Before(next) {
			break
		}
		if i == 0 {
			for _, name := range slices.Sorted(maps.Keys(last)) {
				log.Warn("backup: restore verification failed; will retry", "replica", name, "error", last[name], "retry_in", d)
			}
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return run, false
		case <-t.C:
		}
		for _, name := range slices.Sorted(maps.Keys(last)) {
			err := v.VerifyRestore(ctx, name)
			if ctx.Err() != nil {
				return run, false
			}
			run.attempts[name]++
			if err != nil {
				last[name] = err
				continue
			}
			log.Warn("backup: restore verification passed on retry", "replica", name, "attempts", run.attempts[name], "first_error", first[name])
			delete(last, name)
			run.recovered = append(run.recovered, name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(last)) {
		log.Error("backup: restore verification failed; the replica may not be restorable",
			"replica", name, "attempts", run.attempts[name], "error", last[name], "first_error", first[name])
		run.failures = append(run.failures, VerifyFailure{Name: name, First: first[name], Last: last[name], Attempts: run.attempts[name]})
	}
	run.span = time.Since(start)
	return run, true
}

func (m *Manager) recordVerifyRun(run verifyRun) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, n := range run.attempts {
		v := m.verify[name]
		v.attempts = n
		m.verify[name] = v
	}
	for _, name := range run.recovered {
		v := m.verify[name]
		v.retried++
		m.verify[name] = v
	}
}
