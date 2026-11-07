// Routing of the command line: migrate subcommands reach the migration tool, "migrate --config" the store upgrade.

package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stderrOf runs the command line and returns its exit status and stderr.
func stderrOf(t *testing.T, args ...string) (int, string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = writer
	code := run(args)
	os.Stderr = saved
	writer.Close()
	output, _ := io.ReadAll(reader)
	return code, string(output)
}

func TestRouting(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	cases := []struct {
		args          []string
		want, notWant string
	}{
		{[]string{"migrate", "import"}, "--from is required\nusage: gateway migrate import", ""},
		{[]string{"migrate", "policy", "--exchange"}, "gateway migrate policy", ""},
		{[]string{"migrate", "--config", missing}, "missing.json", "is required"},
		{[]string{"policy", "export-openapi"}, "--policy is required\nusage: gateway policy export-openapi", ""},
	}
	for _, c := range cases {
		code, stderr := stderrOf(t, c.args...)
		if code != 1 || !strings.Contains(stderr, c.want) || c.notWant != "" && strings.Contains(stderr, c.notWant) {
			t.Errorf("%v: exit %d, stderr %q", c.args, code, stderr)
		}
	}
}
