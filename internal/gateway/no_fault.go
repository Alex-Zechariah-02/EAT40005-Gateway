//go:build !faultinject

package gateway

import (
	"net/http"
	"os"
	"time"
)

func crashPoint(string) {}

// registerTestRoutes adds nothing: production binaries have no test hooks.
func registerTestRoutes(*Controller, *http.ServeMux) {}

// writeAudit appends data and syncs it to disk.
func writeAudit(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func rateClockFault() (int64, bool) { return 0, false }

func timerFault() bool { return false }

func rollbackFault() error { return nil }

func candidateReadFault() error { return nil }

func signalCapture(string, string) {}

func wallOffset() time.Duration { return 0 }
