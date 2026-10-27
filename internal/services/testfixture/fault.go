//go:build faultinject

package testfixture

import (
	"net/http"
	"os"
)

// dropAfterWrite loses the acknowledgement only after the service changed
// state and recorded its independent receipt. No response or replay follows.
func dropAfterWrite(w http.ResponseWriter) bool {
	if os.Getenv("FIXTURE_FAULT") != "drop-after-write" {
		return false
	}
	connection, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	connection.Close()
	return true
}
