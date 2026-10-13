// Command pthreadcheck tests whether this sandbox lets C code create threads
// with explicit scheduling attributes, as Odin's thread package does.
package main

/*
#include <pthread.h>
#include <sched.h>
#include <string.h>
#include <errno.h>

static void *noop(void *arg) { return arg; }

static int try_create(int explicit_sched, int set_param, size_t stack) {
	pthread_attr_t attr;
	pthread_attr_init(&attr);
	if (explicit_sched) pthread_attr_setinheritsched(&attr, PTHREAD_EXPLICIT_SCHED);
	if (set_param) {
		int policy = 0;
		struct sched_param p;
		pthread_attr_getschedpolicy(&attr, &policy);
		pthread_attr_getschedparam(&attr, &p);
		p.sched_priority = sched_get_priority_max(policy);
		pthread_attr_setschedparam(&attr, &p);
	}
	if (stack) pthread_attr_setstacksize(&attr, stack);
	pthread_t t;
	int rc = pthread_create(&t, &attr, noop, 0);
	pthread_attr_destroy(&attr);
	if (rc == 0) pthread_join(t, 0);
	return rc;
}
*/
import "C"

import (
	"fmt"
	"syscall"
)

func main() {
	for _, c := range []struct {
		name            string
		explicit, param int
		stack           C.size_t
	}{
		{"default attributes", 0, 0, 0},
		{"explicit sched", 1, 0, 0},
		{"explicit sched, max priority of the policy", 1, 1, 0},
		{"explicit sched, 64 KiB stack", 1, 0, 64 << 10},
	} {
		rc := C.try_create(C.int(c.explicit), C.int(c.param), c.stack)
		fmt.Printf("%-45s rc=%d %v\n", c.name, rc, syscall.Errno(rc))
	}
}
