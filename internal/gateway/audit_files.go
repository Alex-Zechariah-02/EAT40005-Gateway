// Audit file opening, recovery and rotation.

package gateway

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/fault"
)

// openAudit never fails: an unusable file yields a log in the failed state,
// which refuses every acknowledgement and therefore every admission.
func openAudit(path string, maxBytes int64, maxFiles int) *auditLog {
	a := &auditLog{
		boot:     newRequestID()[:16],
		path:     path,
		maxBytes: maxBytes,
		maxFiles: maxFiles,
		queue:    make(chan auditEntry, auditQueueSize),
		done:     make(chan struct{}),
	}
	incomplete, err := a.open()
	if err != nil {
		if a.file != nil {
			a.file.Close()
			a.file = nil
		}
		a.openErr = err
		a.fail(fmt.Errorf("opening audit log: %w", err))
		close(a.done)
		return a
	}
	if incomplete >= 0 {
		// A crash interrupted the last record. It stays in the file for
		// inspection; this event names where it starts.
		a.Record(Event{Kind: "audit.recovered", File: filepath.Base(path), Offset: incomplete})
	}
	go a.run()
	return a
}

// open checks and opens the active file and finds the rotated files. A
// non-empty file that does not end with a newline holds an incomplete record
// from a crash: open terminates it with a newline, so the next record starts
// on its own line, and returns its offset, or -1 when the file is complete.
func (a *auditLog) open() (int64, error) {
	info, err := os.Lstat(a.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return -1, err
	case !info.Mode().IsRegular():
		return -1, fmt.Errorf("%s is not a regular file", a.path)
	}

	incomplete, err := incompleteRecord(a.path)
	if err != nil {
		return -1, err
	}
	a.rotated, a.next, err = rotatedFiles(a.path)
	if err != nil {
		return -1, err
	}
	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return -1, err
	}
	a.file = file
	if info, err = file.Stat(); err != nil {
		return -1, err
	}
	a.size = info.Size()
	if incomplete >= 0 {
		if err := fault.WriteAudit(file, []byte("\n")); err != nil {
			return -1, err
		}
		a.size++
	}
	a.publishUsage()
	return incomplete, nil
}

// incompleteRecord returns the offset of an unterminated last record, or -1.
func incompleteRecord(path string) (int64, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return -1, nil
	}
	if err != nil {
		return -1, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return -1, err
	}
	start := max(info.Size()-maxRecordBytes, 0)
	tail := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(tail, start); err != nil && err != io.EOF {
		return -1, err
	}
	if tail[len(tail)-1] == '\n' {
		return -1, nil
	}
	return start + int64(bytes.LastIndexByte(tail, '\n')+1), nil
}

// rotatedFiles counts the files named "<path>.<n>" and returns the next
// unused n.
func rotatedFiles(path string) (int, int, error) {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return 0, 0, err
	}
	count, next := 0, 1
	prefix := filepath.Base(path) + "."
	for _, entry := range entries {
		suffix, found := strings.CutPrefix(entry.Name(), prefix)
		n, err := strconv.Atoi(suffix)
		if !found || err != nil || n < 1 {
			continue
		}
		count++
		next = max(next, n+1)
	}
	return count, next, nil
}

// rotate closes the active file, renames it to the next rotated name and
// opens a new active file. Every batch was synced when written, so closing
// loses nothing.
func (a *auditLog) rotate() (string, error) {
	if a.rotated >= a.maxFiles-1 {
		return "", fmt.Errorf("audit storage full: %d files of up to %d bytes exist; archive the rotated files and restart", a.maxFiles, a.maxBytes)
	}
	err := a.file.Close()
	a.file = nil
	if err != nil {
		return "", err
	}
	rotated := fmt.Sprintf("%s.%d", a.path, a.next)
	if err := os.Rename(a.path, rotated); err != nil {
		return "", err
	}
	fault.CrashPoint("audit-after-rename")
	file, err := os.OpenFile(a.path, os.O_CREATE|os.O_EXCL|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	a.file, a.size = file, 0
	a.rotated++
	a.next++
	a.publishUsage()
	return filepath.Base(rotated), nil
}
