package main

/*
#cgo LDFLAGS: -ldl
#include <dlfcn.h>
typedef void (*totals_fn)(long *, long *);
static int sync_totals(long *calls, long *nanos) {
	totals_fn f = (totals_fn)dlsym(RTLD_DEFAULT, "syncount_totals");
	if (!f) return 0;
	f(calls, nanos);
	return 1;
}
*/
import "C"

import (
	"time"

	"stardusteval/internal/kanban"
)

// syncTotals reads the counters of trace/syncount.so when it is preloaded.
func syncTotals() (calls int64, spent time.Duration, ok bool) {
	var c, n C.long
	if C.sync_totals(&c, &n) == 0 {
		return 0, 0, false
	}
	return int64(c), time.Duration(n), true
}

// syncWindow counts the syncs inside w and adds them to extra when the
// returned function runs after w ended.
func syncWindow(w kanban.Window) func(extra map[string]float64, transactions int64) {
	var c0 int64
	var s0 time.Duration
	t := time.AfterFunc(time.Until(w.Start), func() { c0, s0, _ = syncTotals() })
	return func(extra map[string]float64, transactions int64) {
		t.Stop()
		c1, s1, ok := syncTotals()
		if !ok || transactions == 0 {
			return
		}
		extra["syncs_per_tx"] = float64(c1-c0) / float64(transactions)
		extra["sync_ms_per_sync"] = float64(s1-s0) / float64(max(c1-c0, 1)) / 1e6
	}
}
