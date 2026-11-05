// Layering of the internal packages: none imports a package above it.

package gateway

import (
	"go/build"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// layers lists, for each internal package, the internal packages it may import.
var layers = map[string][]string{
	"fault":   nil,
	"policy":  nil,
	"compile": {"fault", "policy"},
	"forward": {"compile"},
	"store":   {"fault", "policy"},
	"audit":   {"fault"},
	"openapi": {"policy", "compile"},
	"gateway": {"fault", "policy", "compile", "forward", "store", "audit"},
}

// unlayered packages are not part of the gateway's layering.
var unlayered = []string{"demoports", "services", "services/testfixture"}

func TestPackageLayers(t *testing.T) {
	root := filepath.Join("..", "..")
	module := modulePath(t, root)
	var found []string
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() || path == filepath.Join(root, "internal") {
			return err
		}
		// The go tool ignores these directories, so they hold no package.
		if name := d.Name(); name == "testdata" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
			return filepath.SkipDir
		}
		rel, _ := filepath.Rel(filepath.Join(root, "internal"), path)
		found = append(found, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range found {
		allowed, listed := layers[name]
		if !listed {
			if !slices.Contains(unlayered, name) {
				t.Errorf("internal/%s is not in the layer table", name)
			}
			continue
		}
		for _, tags := range [][]string{nil, {"faultinject"}} {
			ctx := build.Default
			ctx.BuildTags = tags
			pkg, err := ctx.ImportDir(filepath.Join(root, "internal", filepath.FromSlash(name)), 0)
			if err != nil {
				t.Fatalf("internal/%s: %v", name, err)
			}
			for _, imp := range slices.Concat(pkg.Imports, pkg.TestImports) {
				dep, ok := strings.CutPrefix(imp, module+"/internal/")
				if ok && !slices.Contains(allowed, dep) && !slices.Contains(unlayered, dep) {
					t.Errorf("internal/%s imports internal/%s (tags %v)", name, dep, tags)
				}
			}
		}
	}
}

// modulePath reads the module path from go.mod.
func modulePath(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	t.Fatal("go.mod has no module line")
	return ""
}
