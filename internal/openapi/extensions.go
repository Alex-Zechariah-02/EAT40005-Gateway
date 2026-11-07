// The operation-level x-apiward-* extensions a description may carry back from an export.

package openapi

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// CodeExtension refuses an operation whose x-apiward-* member is unknown or invalid.
const CodeExtension = "EXTENSION_INVALID"

// extensionNames are the operation members the import reads; any other
// x-apiward-* member is refused rather than ignored.
var extensionNames = []string{"x-apiward-target", "x-apiward-access", "x-apiward-roles", "x-apiward-rate", "x-apiward-max-bytes", policy.WhenExtension}

// extensions applies the operation's x-apiward-* members to the candidate,
// whose target and widened flag targetOf has already set.
func (m *mapper) extensions(object map[string]any, c *candidate) *refusal {
	for _, name := range sortedKeys(object) {
		if strings.HasPrefix(name, "x-apiward-") && !slices.Contains(extensionNames, name) {
			return refuse(CodeExtension, name, "The member %s is not known.", policy.Clip(name))
		}
	}
	if value, present := object["x-apiward-target"]; present {
		if target, _ := value.(string); target != c.op.Target {
			return refuse(CodeExtension, "x-apiward-target", "x-apiward-target must be %s for this path.", policy.Clip(c.op.Target))
		}
		c.widened = false
		c.entry.Note = "matches deeper paths too, as x-apiward-target states"
	}
	if value, present := object["x-apiward-access"]; present {
		access, _ := value.(string)
		if access != "allow" && access != "deny" {
			return refuse(CodeExtension, "x-apiward-access", "x-apiward-access must be allow or deny.")
		}
		c.op.Access = access
	}
	if value, present := object["x-apiward-rate"]; present {
		rate, refused := rateOf(value)
		if refused != nil {
			return refused
		}
		c.op.Rate = rate
	}
	if value, present := object["x-apiward-max-bytes"]; present {
		number, _ := value.(json.Number)
		size, err := number.Int64()
		if err != nil || size < 1 || size > policy.MaxBodyLimit {
			return refuse(CodeExtension, "x-apiward-max-bytes", "x-apiward-max-bytes must be an integer from 1 to %d.", policy.MaxBodyLimit)
		}
		c.maxBytes = size
	}
	if value, present := object[policy.WhenExtension]; present {
		when, refused := whenOf(value)
		if refused != nil {
			return refused
		}
		c.op.When = when
	}
	if value, present := object["x-apiward-roles"]; present && m.options.WithPermits {
		return rolesOf(value, c)
	}
	return nil
}

// forwardOf reads the root member x-apiward-forward with exactly the
// policy's members, or nil when it is absent; the compiler checks its values.
func forwardOf(root map[string]any) (*policy.Forward, error) {
	value, present := root[policy.ForwardExtension]
	if !present {
		return nil, nil
	}
	refused := errors.New(policy.ForwardExtension + " must be {strip_prefix, add_prefix} as in a policy")
	if _, isObject := value.(map[string]any); !isObject {
		return nil, refused
	}
	data, err := json.Marshal(value)
	var forward policy.Forward
	if err != nil || policy.CheckMembers(data, 1) != nil || policy.DecodeStrict(data, &forward) != nil {
		return nil, refused
	}
	return &forward, nil
}

// rateOf reads a rate rule with exactly the policy's members; the
// compiler checks its values.
func rateOf(value any) (*policy.RateRule, *refusal) {
	refused := refuse(CodeExtension, "x-apiward-rate", "x-apiward-rate must be {algorithm, count, window_seconds, key} as in a policy.")
	if _, isObject := value.(map[string]any); !isObject {
		return nil, refused
	}
	data, err := json.Marshal(value)
	if err != nil || policy.CheckMembers(data, 1) != nil {
		return nil, refused
	}
	var rate policy.RateRule
	if policy.DecodeStrict(data, &rate) != nil {
		return nil, refused
	}
	return &rate, nil
}

// whenOf reads conditions with exactly the policy's members; the compiler
// checks their values. They are checked as the member "when", so the name
// rules apply inside its header list as in a policy.
func whenOf(value any) (*policy.When, *refusal) {
	refused := refuse(CodeExtension, policy.WhenExtension, "%s must be {hosts, headers} as in a policy.", policy.WhenExtension)
	if _, isObject := value.(map[string]any); !isObject {
		return nil, refused
	}
	data, err := json.Marshal(map[string]any{"when": value})
	var holder struct {
		When policy.When `json:"when"`
	}
	if err != nil || policy.CheckMembers(data, 4) != nil || policy.DecodeStrict(data, &holder) != nil {
		return nil, refused
	}
	return &holder.When, nil
}

// rolesOf reads the roles permitted to the operation: distinct identifiers.
func rolesOf(value any, c *candidate) *refusal {
	list, isList := value.([]any)
	if !isList {
		return refuse(CodeExtension, "x-apiward-roles", "x-apiward-roles must be a list of role identifiers.")
	}
	c.roles = []string{}
	for _, item := range list {
		role, _ := item.(string)
		if policy.CheckID(role, "role") != nil || slices.Contains(c.roles, role) {
			return refuse(CodeExtension, "x-apiward-roles", "x-apiward-roles must list distinct valid role identifiers.")
		}
		c.roles = append(c.roles, role)
	}
	return nil
}
