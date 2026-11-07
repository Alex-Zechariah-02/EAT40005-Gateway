// From exchange file to policy: looser settings left out unless accepted, and policy limits named.

package migrate

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// Codes of migrate policy.
const (
	CodeLooser = "LOOSER_NOT_ACCEPTED"   // an operation left out: its ledger item is looser and not accepted
	CodeLimit  = "POLICY_LIMIT_EXCEEDED" // nothing written: the result exceeds a policy limit
)

// methods an operation pointer may name, as OpenAPI lists them.
var methods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

// withoutLooser returns a copy of the document without each operation that
// a looser, unaccepted ledger item points into, and a refusal for each. A
// looser item that points at no operation is an error: it must not pass.
func withoutLooser(document map[string]any, ledger *exchange.Ledger) (map[string]any, []openapi.Refused, error) {
	paths, _ := document["paths"].(map[string]any)
	paths = maps.Clone(paths)
	refused := []openapi.Refused{}
	removed := map[string]bool{}
	for _, item := range ledger.Items {
		if item.Class != exchange.Approximated || item.Direction != exchange.Looser || item.Accepted {
			continue
		}
		path, method, err := operationAt(item.Target)
		if removed[method+" "+path] {
			continue
		}
		pathItem, _ := paths[path].(map[string]any)
		if _, present := pathItem[method]; err != nil || !present {
			return nil, nil, fmt.Errorf("ledger item %s points to no operation (%s)", policy.Clip(item.ID), policy.Clip(item.Target))
		}
		pathItem = maps.Clone(pathItem)
		delete(pathItem, method)
		paths[path], removed[method+" "+path] = pathItem, true
		refused = append(refused, openapi.Refused{Method: strings.ToUpper(method), Path: policy.Clip(path), Code: CodeLooser,
			Message: "Looser than the source: " + item.Note + " Accept the ledger item to import it.", Field: policy.Clip(item.ID)})
	}
	out := maps.Clone(document)
	out["paths"] = paths
	return out, refused, nil
}

// operationAt reads the path and method of a pointer #/paths/<path>/<method>[/…].
func operationAt(pointer string) (string, string, error) {
	tokens := strings.Split(strings.TrimPrefix(pointer, "#/paths/"), "/")
	if !strings.HasPrefix(pointer, "#/paths/") || len(tokens) < 2 || !slices.Contains(methods, tokens[1]) {
		return "", "", errors.New("not an operation pointer")
	}
	path := strings.NewReplacer("~1", "/", "~0", "~").Replace(tokens[0])
	return path, tokens[1], nil
}

// limitError names the policy limit a result exceeds; other errors pass unchanged.
func limitError(err error) error {
	var invalid *policy.PolicyError
	if !errors.As(err, &invalid) || invalid.Code != "LIMIT_EXCEEDED" {
		return err
	}
	limits := map[string]string{
		"size":       fmt.Sprintf("at most %d bytes", policy.MaxPolicyBytes),
		"upstreams":  fmt.Sprintf("at most %d", policy.MaxUpstreams),
		"apis":       fmt.Sprintf("at most %d", policy.MaxAPIs),
		"operations": fmt.Sprintf("at most %d", policy.MaxOperations),
		"roles":      fmt.Sprintf("at most %d", policy.MaxRoles),
		"permits":    fmt.Sprintf("at most %d", policy.MaxPermits),
		"":           fmt.Sprintf("nesting at most %d levels", policy.MaxPolicyDepth),
	}
	limit, known := limits[invalid.Field]
	if !known {
		return err
	}
	return fmt.Errorf("%s: %s: %s; import fewer operations or another API on its own", CodeLimit, cmp.Or(invalid.Field, "nesting"), limit)
}
