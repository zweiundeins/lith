package writerrace

import "syscall"

// lowerPriority lowers the calling thread's CPU priority to nice 10 and
// its I/O priority to the lowest of the best-effort class, so that the
// race's writer yields to the site and to the host's other services
// whenever they want the CPU or the disk.
func lowerPriority() error {
	tid := syscall.Gettid()
	if err := syscall.Setpriority(syscall.PRIO_PROCESS, tid, 10); err != nil {
		return err
	}
	const whoProcess, classBestEffort, classShift, lowest = 1, 2, 13, 7
	if _, _, e := syscall.Syscall(syscall.SYS_IOPRIO_SET, whoProcess, uintptr(tid), classBestEffort<<classShift|lowest); e != 0 {
		return e
	}
	return nil
}
