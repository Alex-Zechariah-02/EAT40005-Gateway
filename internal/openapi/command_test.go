// Command line: flags, exit codes, never-overwrite and limits.

package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

type commandRun struct {
	dir, spec, base, out, report string
}

func newRun(t *testing.T, specName string, spec []byte) commandRun {
	t.Helper()
	dir := t.TempDir()
	run := commandRun{
		dir:    dir,
		spec:   filepath.Join(dir, specName),
		base:   filepath.Join(dir, "base.json"),
		out:    filepath.Join(dir, "out.json"),
		report: filepath.Join(dir, "report.json"),
	}
	base, err := json.Marshal(testBase())
	if err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{run.spec: spec, run.base: base} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return run
}

func (r commandRun) args(extra ...string) []string {
	args := []string{"import-openapi", "--spec", r.spec, "--api", "orders", "--upstream", "orders-svc",
		"--base", r.base, "--out", r.out, "--report", r.report}
	return append(args, extra...)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestImportCommand(t *testing.T) {
	t.Run("writes policy and report", func(t *testing.T) {
		run := newRun(t, "orders.yaml", fixture(t, "orders.yaml"))
		var stderr bytes.Buffer
		if code := Command(run.args(), &stderr); code != ExitPartial {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		document, _ := os.ReadFile(run.out)
		if _, err := policy.DecodePolicy(document); err != nil || !bytes.HasSuffix(document, []byte("}\n")) {
			t.Fatalf("policy file: %v", err)
		}
		raw, _ := os.ReadFile(run.report)
		var report Report
		if err := json.Unmarshal(raw, &report); err != nil || report.SourceFormat != "yaml" || len(report.Refused) != 2 {
			t.Fatalf("report: %v %+v", err, report)
		}
		if !strings.Contains(stderr.String(), "6 of 8 operations") {
			t.Fatalf("summary: %q", stderr.String())
		}
	})

	t.Run("everything mapped exits 0", func(t *testing.T) {
		spec := `{"openapi":"3.1.0","info":{"title":"t","version":"1"},"paths":{"/orders":{"get":{"operationId":"list","security":[{"b":[]}]}}},"components":{"securitySchemes":{"b":{"type":"http","scheme":"bearer"}}}}`
		run := newRun(t, "spec.JSON", []byte(spec))
		if code := Command(run.args("--max-body-bytes", "1048576"), new(bytes.Buffer)); code != ExitComplete {
			t.Fatalf("exit %d", code)
		}
	})

	t.Run("nothing written", func(t *testing.T) {
		big := append([]byte(`{"openapi":"3.1.0","x":"`), bytes.Repeat([]byte("x"), MaxSpecBytes)...)
		big = append(big, `"}`...)
		cases := []struct {
			name, specName string
			spec           []byte
			args           func(commandRun) []string
			want           string
		}{
			{"no subcommand", "s.json", nil, func(r commandRun) []string { return nil }, "usage"},
			{"unknown subcommand", "s.json", nil, func(r commandRun) []string { return []string{"export"} }, "usage"},
			{"missing flag", "s.json", nil, func(r commandRun) []string { return r.args()[:11] }, "--report is required"},
			{"repeated flag", "s.json", nil, func(r commandRun) []string { return r.args("--api", "other") }, "more than once"},
			{"extra argument", "s.json", nil, func(r commandRun) []string { return r.args("extra") }, "usage"},
			{"unknown flag", "s.json", nil, func(r commandRun) []string { return r.args("--dial", "x") }, "usage"},
			{"file type", "s.txt", nil, func(r commandRun) []string { return r.args() }, "unsupported file type"},
			{"spec too large", "s.json", big, func(r commandRun) []string { return r.args() }, "1 MiB"},
			{"missing spec", "s.json", nil, func(r commandRun) []string { a := r.args(); a[2] = filepath.Join(r.dir, "gone.json"); return a }, "spec"},
			{"body limit zero", "s.json", nil, func(r commandRun) []string { return r.args("--max-body-bytes", "0") }, "max-body-bytes"},
			{"body limit text", "s.json", nil, func(r commandRun) []string { return r.args("--max-body-bytes", "lots") }, "max-body-bytes"},
			{"body limit high", "s.json", nil, func(r commandRun) []string { return r.args("--max-body-bytes", "1048577") }, "max-body-bytes"},
			{"bad base", "s.json", nil, func(r commandRun) []string { os.WriteFile(r.base, []byte("{}"), 0o600); return r.args() }, "base"},
			{"bad spec", "s.json", []byte("{"), func(r commandRun) []string { return r.args() }, "malformed_json"},
		}
		for _, c := range cases {
			spec := c.spec
			if spec == nil {
				spec = fixture(t, "orders.json")
			}
			run := newRun(t, c.specName, spec)
			var stderr bytes.Buffer
			code := Command(c.args(run), &stderr)
			if code != ExitFailed || !strings.Contains(stderr.String(), c.want) {
				t.Errorf("%s: exit %d, stderr %q, want %q", c.name, code, stderr.String(), c.want)
			}
			if exists(run.out) || exists(run.report) {
				t.Errorf("%s: a file was written", c.name)
			}
		}
	})

	t.Run("never overwrites", func(t *testing.T) {
		for _, target := range []string{"out", "report"} {
			run := newRun(t, "orders.json", fixture(t, "orders.json"))
			existing, other := run.out, run.report
			if target == "report" {
				existing, other = run.report, run.out
			}
			if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			if code := Command(run.args(), &stderr); code != ExitFailed || !strings.Contains(stderr.String(), "already exists") {
				t.Fatalf("%s: exit %d, %q", target, code, stderr.String())
			}
			if kept, _ := os.ReadFile(existing); string(kept) != "keep" || exists(other) {
				t.Fatalf("%s: existing file changed or the other file was written", target)
			}
		}
	})

	t.Run("compile failure writes nothing", func(t *testing.T) {
		run := newRun(t, "orders.json", fixture(t, "orders.json"))
		full := testBase()
		full.Operations, full.Permits = nil, nil
		for i := range policy.MaxOperations - 1 {
			full.Operations = append(full.Operations, policy.Operation{ID: fmt.Sprintf("inventory.op%d", i),
				API: "inventory", Method: "GET", Target: fmt.Sprintf("/inventory/%d", i), Access: "allow", Auth: "required"})
		}
		base, _ := json.Marshal(full)
		os.WriteFile(run.base, base, 0o600)
		var stderr bytes.Buffer
		if code := Command(run.args(), &stderr); code != ExitFailed || !strings.Contains(stderr.String(), "LIMIT_EXCEEDED") {
			t.Fatalf("exit %d, %q", code, stderr.String())
		}
		if exists(run.out) || exists(run.report) {
			t.Fatal("a file was written")
		}
	})
}
