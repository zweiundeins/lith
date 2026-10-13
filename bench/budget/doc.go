// Package budget measures what a stream's encoder costs under each
// compression setting, for sse.Budget's tiers: the heap a stream holds
// after a session, the bytes each frame takes on the wire and the CPU
// each frame takes, on two workloads. It also simulates budgets as streams
// grow, measures idle hibernation, and runs the board load test with and
// without a budget. The CSV files next to this one are its output.
//
// The measurements run only on request, pinned to the performance cores:
//
//	LITH_BUDGET=1 taskset -c 0-11 go test ./bench/budget -run Settings -v -timeout 1h
package budget
