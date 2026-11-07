// Import: a description plus a base policy in, a compiled candidate policy and its report out.

package openapi

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/compile"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// DefaultBodyBytes is the body limit given to imported operations unless
// the caller sets one; it is reported as assumed.
const DefaultBodyBytes = 16384

var supportedVersion = regexp.MustCompile(`^3\.[01]\.[0-9]+$`)

// Options are the administrator's choices for one import.
type Options struct {
	API          string        // the API to create or replace
	Upstream     string        // an upstream of Base that serves the API
	Base         policy.Policy // the policy the result is built on
	MaxBodyBytes int64         // body limit of body rules without x-apiward-max-bytes
	WithPermits  bool          // read x-apiward-roles into permits
}

func (o Options) check() error {
	if policy.CheckID(o.API, "api") != nil {
		return fmt.Errorf("api %q is not a valid identifier", policy.Clip(o.API))
	}
	if !slices.ContainsFunc(o.Base.Upstreams, func(u policy.Upstream) bool { return u.ID == o.Upstream }) {
		return fmt.Errorf("upstream %q is not in the base policy", policy.Clip(o.Upstream))
	}
	if o.MaxBodyBytes < 1 || o.MaxBodyBytes > policy.MaxBodyLimit {
		return fmt.Errorf("max-body-bytes must be 1 to %d", policy.MaxBodyLimit)
	}
	return nil
}

// Import maps a description onto the base policy. It returns the result,
// which the real decoder and compiler have accepted, and the report; or an
// error, in which case nothing may be written. It has no side effects.
func Import(spec []byte, format Format, options Options) (policy.Policy, *Report, error) {
	value, err := Decode(spec, format, MaxSpecBytes)
	if err != nil {
		return policy.Policy{}, nil, err
	}
	root, ok := value.(map[string]any)
	if !ok {
		return policy.Policy{}, nil, errors.New("the description is not a JSON object")
	}
	return ImportDocument(root, format, options)
}

// ImportDocument is Import for a description already passed through Decode.
func ImportDocument(root map[string]any, format Format, options Options) (policy.Policy, *Report, error) {
	if err := options.check(); err != nil {
		return policy.Policy{}, nil, err
	}
	result, report, err := assemble(root, format, options)
	if err != nil {
		return policy.Policy{}, nil, err
	}
	if err := compiles(result); err != nil {
		return policy.Policy{}, nil, err
	}
	return result, report, nil
}

// classifyAPI and classifyUpstream stand in for the administrator's choices
// when a description is classified without a base policy.
const (
	classifyAPI      = "x"
	classifyUpstream = "upstream"
)

// Classify reports what an import would do with each operation of a
// description already passed through Decode, without a base policy and
// without the limits on a whole policy. Operations are named by operationId
// alone and the report's api is empty.
func Classify(root map[string]any, format Format) (*Report, error) {
	options := Options{API: classifyAPI, Upstream: classifyUpstream, MaxBodyBytes: DefaultBodyBytes, WithPermits: true,
		Base: policy.Policy{Upstreams: []policy.Upstream{{ID: classifyUpstream, Dial: "127.0.0.1:1"}}}}
	_, report, err := assemble(root, format, options)
	if err != nil {
		return nil, err
	}
	report.unprefix(classifyAPI + ".")
	return report, nil
}

// assemble maps every operation onto the base without the API being
// replaced and returns the result before the whole-policy checks.
func assemble(root map[string]any, format Format, options Options) (policy.Policy, *Report, error) {
	version, _ := root["openapi"].(string)
	if !supportedVersion.MatchString(version) {
		return policy.Policy{}, nil, fmt.Errorf("unsupported openapi version %q: 3.0.x or 3.1.x is required", policy.Clip(version))
	}
	settings, err := settingsOf(root)
	if err != nil {
		return policy.Policy{}, nil, err
	}
	report := newReport(format, options.API)
	report.SpecVersion = version
	m := &mapper{root: root, options: options, report: report, ignored: map[string]bool{}}
	found := m.operations()

	result := replaceAPI(options, settings, report)
	for _, c := range m.crossCheck(found, result.Operations) {
		accept(c, &result, report)
	}
	for name := range m.ignored {
		report.Ignored = append(report.Ignored, name)
	}
	slices.Sort(report.Ignored)
	slices.Sort(report.ScopesNotMapped)
	report.ScopesNotMapped = slices.Compact(report.ScopesNotMapped)
	return result, report, nil
}

// replaceAPI copies the base without the API being imported, its
// operations, and the permits that name them; each removal is reported. The
// API is added again with the description's settings (forward rule, browser
// access, response headers, address allow list), or none.
func replaceAPI(options Options, settings policy.API, report *Report) policy.Policy {
	base := options.Base
	result := base
	result.Upstreams = slices.Clone(base.Upstreams)
	result.Roles = slices.Clone(base.Roles)
	result.APIs = nil
	for _, api := range base.APIs {
		if api.ID != options.API {
			result.APIs = append(result.APIs, api)
		}
	}
	settings.ID, settings.Upstream = options.API, options.Upstream
	result.APIs = append(result.APIs, settings)
	removed := map[string]bool{}
	result.Operations = nil
	for _, op := range base.Operations {
		if op.API == options.API {
			removed[op.ID] = true
			report.RemovedOperations = append(report.RemovedOperations, op.ID)
			continue
		}
		result.Operations = append(result.Operations, op)
	}
	result.Permits = nil
	for _, permit := range base.Permits {
		if removed[permit.Operation] {
			report.RemovedPermits = append(report.RemovedPermits, Permit{Role: permit.Role, Operation: permit.Operation})
			continue
		}
		result.Permits = append(result.Permits, permit)
	}
	return result
}

// crossCheck refuses every operation whose identifier, or method and
// target, is shared with another candidate or a kept base operation. It
// returns the candidates left.
func (m *mapper) crossCheck(found []*candidate, kept []policy.Operation) []*candidate {
	ids, routes := map[string]int{}, map[string]int{}
	for _, op := range kept {
		ids[op.ID]++
		routes[op.Method+" "+op.Target]++
	}
	for _, c := range found {
		ids[c.op.ID]++
		routes[c.op.Method+" "+c.op.Target]++
	}
	var accepted []*candidate
	for _, c := range found {
		switch {
		case ids[c.op.ID] > 1:
			m.refuse(c.entry.Method, c.entry.Path, refuse(CodeDuplicateID, "", "Another operation has the identifier %s.", c.op.ID))
		case routes[c.op.Method+" "+c.op.Target] > 1:
			m.refuse(c.entry.Method, c.entry.Path, refuse(CodeTargetCollision, "", "Another operation maps to %s %s.", c.op.Method, policy.Clip(c.op.Target)))
		default:
			accepted = append(accepted, c)
		}
	}
	return accepted
}

// accept adds a checked operation to the result and its entries to the report.
func accept(c *candidate, result *policy.Policy, report *Report) {
	result.Operations = append(result.Operations, c.op)
	if c.widened {
		report.Widened = append(report.Widened, c.entry)
	} else {
		report.Mapped = append(report.Mapped, c.entry)
	}
	if c.op.Auth == "required" && c.roles == nil {
		report.NeedsPermissions = append(report.NeedsPermissions, c.op.ID)
	}
	for _, role := range c.roles {
		result.Permits = append(result.Permits, policy.Permit{Role: role, Operation: c.op.ID})
		if !slices.ContainsFunc(result.Roles, func(r policy.Role) bool { return r.ID == role }) {
			result.Roles = append(result.Roles, policy.Role{ID: role})
		}
	}
	report.Assumed = append(report.Assumed, c.assumed...)
	report.Changed = append(report.Changed, c.changed...)
	report.NotEnforced = append(report.NotEnforced, c.notEnforced...)
	report.AnnotationsRemoved = append(report.AnnotationsRemoved, c.annotations...)
	report.ScopesNotMapped = append(report.ScopesNotMapped, c.scopes...)
}

// compiles runs the result through what staging runs, on the bytes that
// are written: the size limit, strict decoding, then the compiler.
func compiles(result policy.Policy) error {
	document, err := indent(result)
	if err != nil {
		return err
	}
	if len(document) > policy.MaxPolicyBytes {
		err = policy.Invalid("LIMIT_EXCEEDED", "size")
	} else {
		var decoded policy.Policy
		if decoded, err = policy.DecodePolicy(document); err == nil {
			_, err = compile.Compile(decoded, 0, time.Time{})
		}
	}
	if err != nil {
		return fmt.Errorf("the result is not a valid policy: %w", err)
	}
	return nil
}
