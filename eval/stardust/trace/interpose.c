// LD_PRELOAD shim that logs the libc calls libstardust makes, to find where a
// commit blocks without strace or gdb.
#define _GNU_SOURCE
#include <dlfcn.h>
#include <pthread.h>
#include <signal.h>
#include <stdarg.h>
#include <stdio.h>
#include <sys/time.h>
#include <unistd.h>
#include <sched.h>
#include <sys/syscall.h>

#define LOG(...) do { fprintf(stderr, "[shim %ld] ", (long)syscall(SYS_gettid)); fprintf(stderr, __VA_ARGS__); fputc('\n', stderr); } while (0)

int pthread_create(pthread_t *t, const pthread_attr_t *a, void *(*fn)(void *), void *arg) {
	static int (*real)(pthread_t *, const pthread_attr_t *, void *(*)(void *), void *);
	if (!real) real = dlsym(RTLD_NEXT, "pthread_create");
	int rc = real(t, a, fn, arg);
	Dl_info info = {0};
	dladdr((void *)fn, &info);
	if (a) {
		int inh = -1, pol = -1, det = -1; size_t st = 0, gs = 0; struct sched_param sp = {0};
		pthread_attr_getinheritsched(a, &inh); pthread_attr_getschedpolicy(a, &pol);
		pthread_attr_getschedparam(a, &sp); pthread_attr_getstacksize(a, &st);
		pthread_attr_getguardsize(a, &gs); pthread_attr_getdetachstate(a, &det);
		LOG("pthread_create fn=%s rc=%d inherit=%d policy=%d prio=%d stack=%zu guard=%zu detach=%d", info.dli_fname ? info.dli_fname : "?", rc, inh, pol, sp.sched_priority, st, gs, det);
	} else LOG("pthread_create fn=%s rc=%d (no attr)", info.dli_fname ? info.dli_fname : "?", rc);
	return rc;
}

int sigaction(int sig, const struct sigaction *act, struct sigaction *old) {
	static int (*real)(int, const struct sigaction *, struct sigaction *);
	if (!real) real = dlsym(RTLD_NEXT, "sigaction");
	Dl_info info = {0};
	dladdr(__builtin_return_address(0), &info);
	int rc = real(sig, act, old);
	if (act && info.dli_fname && __builtin_strstr(info.dli_fname, "stardust")) LOG("sigaction sig=%d rc=%d", sig, rc);
	return rc;
}

int setitimer(__itimer_which_t which, const struct itimerval *restrict nv, struct itimerval *restrict ov) {
	static int (*real)(__itimer_which_t, const struct itimerval *, struct itimerval *);
	if (!real) real = dlsym(RTLD_NEXT, "setitimer");
	int rc = real(which, nv, ov);
	LOG("setitimer which=%d interval=%ld.%06ld value=%ld.%06ld rc=%d", which,
		nv ? (long)nv->it_interval.tv_sec : -1L, nv ? (long)nv->it_interval.tv_usec : 0L,
		nv ? (long)nv->it_value.tv_sec : -1L, nv ? (long)nv->it_value.tv_usec : 0L, rc);
	return rc;
}

long syscall(long n, ...) {
	static long (*real)(long, ...);
	if (!real) real = dlsym(RTLD_NEXT, "syscall");
	va_list ap;
	va_start(ap, n);
	long a[6];
	for (int i = 0; i < 6; i++) a[i] = va_arg(ap, long);
	va_end(ap);
	if (n != SYS_gettid) LOG("syscall %ld (%ld, %ld, %ld) ...", n, a[0], a[1], a[2]);
	long rc = real(n, a[0], a[1], a[2], a[3], a[4], a[5]);
	if (n != SYS_gettid) LOG("syscall %ld -> %ld", n, rc);
	return rc;
}

int fdatasync(int fd) {
	static int (*real)(int);
	if (!real) real = dlsym(RTLD_NEXT, "fdatasync");
	LOG("fdatasync fd=%d ...", fd);
	int rc = real(fd);
	LOG("fdatasync fd=%d -> %d", fd, rc);
	return rc;
}

int flock(int fd, int op) {
	static int (*real)(int, int);
	if (!real) real = dlsym(RTLD_NEXT, "flock");
	int rc = real(fd, op);
	LOG("flock fd=%d op=%d -> %d", fd, op, rc);
	return rc;
}

FILE *popen(const char *cmd, const char *mode) {
	LOG("popen called: refused");
	return NULL;
}

int system(const char *cmd) {
	LOG("system called: refused");
	return -1;
}
