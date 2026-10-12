//go:build !unix

package loadtest

import "time"

func cpuTime() time.Duration { return 0 }
