package backup

import (
	"context"
	"fmt"
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
	// about 10 and 30 minutes after the run; an empty slice means none. A
	// zero or negative wait retries at once. A retry that would start at or
	// after the next run's start is skipped, with every one after it. For a
	// provider whose incidents last hours, stretch them toward the interval,
	// such as 30m, 2h and 6h against 24h.
	Retries []time.Duration

	// Names, if set, lists the replicas each run verifies, in place of the
	// attached ones: an app that attaches its tenants on demand passes its
	// account list, since a tenant not attached at the time is otherwise
	// never verified. Each is verified by name with VerifyRestore, which
	// attaches nothing; a name with nothing replicated under it fails. Its
	// results show in Status and the metrics once the replica is attached.
	// When Names fails, the run verifies nothing and OnFail gets one failure
	// with an empty Name and Names' error.
	Names func(ctx context.Context) ([]string, error)

	// OnFail, if set, is called once per run, after its retries, with the
	// replicas that failed every attempt, sorted by name, and the run's span:
	// the time from its start to the end of its last attempt. A run in which
	// every replica passed, at once or on a retry, calls nothing, and a
	// replica purged during the run is left out. A panic in OnFail is logged
	// and the loop goes on.
	OnFail func(failures []VerifyFailure, span time.Duration)
}

// VerifyFailure is a replica that failed every verification of a run. First
// and Last are the errors of its first and last attempt. The failures of one
// run share Attempts, since each went through every retry.
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
	purgeMark() uint64
	purgedSince(name string, mark uint64) bool
}

// verifyRun is the outcome of one run with its retries.
type verifyRun struct {
	mark      uint64 // purgeMark before the run's first attempt
	attempts  map[string]int
	recovered []string // failed, then passed on a retry
	failures  []VerifyFailure
	span      time.Duration
}

// RunVerifyLoop verifies every attached replica, or opt.Names, until ctx is
// done: the first run after at most ten minutes, then one every opt.Interval
// from the start of the one before. The replicas a run finds failing are
// verified again after each of opt.Retries, and those that never pass go to
// opt.OnFail, once per run. When ctx ends during a run, the run reports
// nothing.
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
		run, ok := verifyWithRetries(ctx, v, log, opt.Names, retries, start, next)
		if !ok {
			return
		}
		record(run)
		if len(run.failures) > 0 && opt.OnFail != nil {
			callOnFail(log, opt.OnFail, run)
		}
		timer.Reset(time.Until(next))
	}
}

func callOnFail(log *slog.Logger, onFail func([]VerifyFailure, time.Duration), run verifyRun) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("backup: OnFail panicked", "panic", r)
		}
	}()
	onFail(run.failures, run.span)
}

// verifyWithRetries runs one verification of every replica and retries the
// failed ones while a retry fits before next. ok is false when ctx ended.
func verifyWithRetries(ctx context.Context, v verifier, log *slog.Logger, names func(context.Context) ([]string, error), retries []time.Duration, start, next time.Time) (run verifyRun, ok bool) {
	run.attempts = map[string]int{}
	run.mark = v.purgeMark()
	var results map[string]error
	if names == nil {
		results = v.VerifyAll(ctx)
	} else {
		list, err := names(ctx)
		if ctx.Err() != nil {
			return run, false
		}
		if err != nil {
			err = fmt.Errorf("backup: list the replicas to verify: %w", err)
			log.Error("backup: no restore verified in this run", "error", err)
			run.failures = []VerifyFailure{{First: err, Last: err, Attempts: 1}}
			run.span = time.Since(start)
			return run, true
		}
		results = map[string]error{}
		for _, name := range list {
			if ctx.Err() != nil {
				break
			}
			if _, done := results[name]; !done {
				results[name] = v.VerifyRestore(ctx, name)
			}
		}
	}
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
	dropPurged := func() {
		for name := range last {
			if v.purgedSince(name, run.mark) {
				delete(last, name)
			}
		}
	}
	for i, d := range retries {
		dropPurged()
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
		dropPurged()
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
	dropPurged()
	for _, name := range slices.Sorted(maps.Keys(last)) {
		log.Error("backup: restore verification failed; the replica may not be restorable",
			"replica", name, "attempts", run.attempts[name], "error", last[name], "first_error", first[name])
		run.failures = append(run.failures, VerifyFailure{Name: name, First: first[name], Last: last[name], Attempts: run.attempts[name]})
	}
	run.span = time.Since(start)
	return run, true
}

// purgeMark numbers the purges so far, for purgedSince.
func (m *Manager) purgeMark() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.purgeSeq
}

// purgedSince reports whether name was purged after purgeMark returned mark.
func (m *Manager) purgedSince(name string, mark uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.purges[name] > mark
}

func (m *Manager) recordVerifyRun(run verifyRun) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, n := range run.attempts {
		if m.purges[name] > run.mark {
			continue
		}
		v := m.verify[name]
		v.attempts = n
		m.verify[name] = v
	}
	for _, name := range run.recovered {
		if m.purges[name] > run.mark {
			continue
		}
		v := m.verify[name]
		v.retried++
		m.verify[name] = v
	}
}
