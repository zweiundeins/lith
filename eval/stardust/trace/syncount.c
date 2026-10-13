// LD_PRELOAD shim that counts fdatasync and fsync calls and their time, and
// prints the totals when the process exits.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdatomic.h>
#include <stdio.h>
#include <time.h>

static atomic_long calls, nanos;

static long now(void) {
	struct timespec ts;
	clock_gettime(CLOCK_MONOTONIC, &ts);
	return ts.tv_sec * 1000000000L + ts.tv_nsec;
}

static int timed(int (*real)(int), int fd) {
	long start = now();
	int rc = real(fd);
	atomic_fetch_add(&calls, 1);
	atomic_fetch_add(&nanos, now() - start);
	return rc;
}

int fdatasync(int fd) {
	static int (*real)(int);
	if (!real) real = dlsym(RTLD_NEXT, "fdatasync");
	return timed(real, fd);
}

int fsync(int fd) {
	static int (*real)(int);
	if (!real) real = dlsym(RTLD_NEXT, "fsync");
	return timed(real, fd);
}

void syncount_totals(long *c, long *n) {
	*c = atomic_load(&calls);
	*n = atomic_load(&nanos);
}
