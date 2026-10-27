package main

import (
	"math"
	"testing"
	"time"
)

func TestSampleClockResolvesShortIntervals(t *testing.T) {
	clock, err := newSampleClock()
	if err != nil {
		t.Fatal(err)
	}
	if clock.frequency < 1e6 {
		t.Fatal("measurement clock has insufficient nominal resolution")
	}
	smallest := math.MaxFloat64
	deadline := time.Now().Add(time.Second)
	previous, err := clock.read()
	if err != nil {
		t.Fatal(err)
	}
	changes := 0
	for changes < 32 && time.Now().Before(deadline) {
		current, err := clock.read()
		if err != nil {
			t.Fatal(err)
		}
		if current < previous {
			t.Fatal("measurement clock moved backwards")
		}
		if current > previous {
			smallest = min(smallest, float64(current-previous)/clock.frequency)
			changes++
		}
		previous = current
	}
	if changes != 32 || smallest >= 0.0001 {
		t.Fatalf("clock cannot resolve short requests: changes=%d smallest_seconds=%g", changes, smallest)
	}
	t.Logf("clock=%s frequency_hz=%.0f smallest_observed_us=%.3f", clock.name, clock.frequency, smallest*1e6)
}
