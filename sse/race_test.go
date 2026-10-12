//go:build race

package sse

// The race detector slows compression about thirtyfold.
const heapStreams = 5
