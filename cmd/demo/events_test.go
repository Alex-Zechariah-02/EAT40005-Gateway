package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readEvents(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	stdout, stderr := os.Stdout, os.Stderr
	outRead, outWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	errRead, errWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = outWrite, errWrite
	defer func() { os.Stdout, os.Stderr = stdout, stderr }()
	err = events(args)
	outWrite.Close()
	errWrite.Close()
	output, outErr := io.ReadAll(outRead)
	diagnostic, diagErr := io.ReadAll(errRead)
	outRead.Close()
	errRead.Close()
	if outErr != nil || diagErr != nil {
		t.Fatalf("reading events output: %v %v", outErr, diagErr)
	}
	return string(output), string(diagnostic), err
}

func TestEventsActiveFileContract(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, auditFile)
	lines := `{"kind":"request.rejected","request_id":"one","stage":"JWT","authorization":"secret-value"}
{"kind":"transition.committed"}
bad complete record
{"kind":"request.forwarded","request_id":"two","status":200}
{"kind":"request.rejected","request_id":"one","status":403}
{"kind":`
	if err := os.WriteFile(active, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(active+".1", []byte(`{"kind":"request.forwarded","request_id":"rotated"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"last", []string{"-last", "2"}, []string{"two", "one"}},
		{"request", []string{"-request", "one", "-last", "1"}, []string{"one", "one"}},
		{"missing", []string{"-request", "absent"}, nil},
		{"rotated", []string{"-request", "rotated"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"-dir", dir}, tc.args...)
			output, diagnostic, err := readEvents(t, args...)
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Split(strings.TrimSpace(output), "\n")
			if len(tc.want) == 0 {
				if output != "" || !strings.Contains(diagnostic, "does not establish non-arrival") {
					t.Fatalf("empty history: %q %q", output, diagnostic)
				}
			} else {
				if len(got) != len(tc.want) {
					t.Fatalf("events %q, want %v", output, tc.want)
				}
				for i, id := range tc.want {
					if !strings.Contains(got[i], "request_id="+id) {
						t.Fatalf("event order %q, want %v", output, tc.want)
					}
				}
			}
			if strings.Contains(output+diagnostic, "secret-value") || !strings.Contains(diagnostic, "rotated files were not searched") || !strings.Contains(diagnostic, "skipped 2") {
				t.Fatalf("secret or inaccurate history diagnostics: %q %q", output, diagnostic)
			}
		})
	}
	if err := os.Remove(active); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readEvents(t, "-dir", dir); err == nil {
		t.Fatal("missing active audit file was accepted")
	}
	if err := os.Mkdir(active, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readEvents(t, "-dir", dir); err == nil {
		t.Fatal("directory used as audit file was accepted")
	}
	if _, _, err := readEvents(t, "-dir", dir, "-last", "0"); err == nil {
		t.Fatal("invalid event limit was accepted")
	}
}
