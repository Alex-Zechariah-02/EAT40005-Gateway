//go:build !faultinject

// No-op fault points for builds without the faultinject tag.

package fault

import (
	"context"
	"os"
	"time"
)

func CancelPoint(context.Context, string) {}

func CrashPoint(string) {}

// WriteAudit appends data and syncs it to disk.
func WriteAudit(file *os.File, data []byte) error {
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func RateClockFault() (int64, bool) { return 0, false }

func TimerFault() bool { return false }

func RollbackFault() error { return nil }

func CandidateReadFault() error { return nil }

func SignalCapture(string, string) {}

func WallOffset() time.Duration { return 0 }
