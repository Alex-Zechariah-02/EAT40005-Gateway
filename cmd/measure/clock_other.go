//go:build !windows

package main

import "time"

func newSampleClock() (sampleClock, error) {
	origin := time.Now()
	return sampleClock{name: "Go monotonic", frequency: 1e9, read: func() (int64, error) { return time.Since(origin).Nanoseconds(), nil }}, nil
}
