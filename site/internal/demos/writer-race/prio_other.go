//go:build !linux

package writerrace

// lowerPriority does nothing outside Linux.
func lowerPriority() error { return nil }
