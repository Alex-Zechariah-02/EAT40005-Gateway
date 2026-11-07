// Test helpers: reading the hand-written decK fixtures through the import's gate.

package kong

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
)

// fixture reads a decK file from testdata through the YAML gate.
func fixture(t *testing.T, name string) (map[string]any, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	value, err := openapi.Decode(data, openapi.YAML, 8<<20)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return value.(map[string]any), data
}

// read reads a fixture and fails the test if the reader refuses it.
func read(t *testing.T, name string) *Result {
	t.Helper()
	root, data := fixture(t, name)
	result, err := Read(root, data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return result
}
