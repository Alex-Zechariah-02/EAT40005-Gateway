package main

// The measurement clock is separate from request timeouts. Windows' Go runtime
// clock reads interrupt time, which can quantize short local requests to zero.
type sampleClock struct {
	name      string
	frequency float64
	read      func() (int64, error)
}
