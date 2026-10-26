package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// compilerVersion is part of every generation identity so that a change in
// compilation rules is visible even when the policy revision is unchanged.
const compilerVersion = "4"

// schemaVersion is the only policy document version this compiler accepts.
const schemaVersion = 3

// profileCore is the only enforcement profile that can be activated. Under it
// the gateway enforces routing, the per-operation access flag, bearer JWT
// authentication and role permits on every operation declared auth
// "required", the operation's rate rule, its body and header schemas, and the
// mandatory audit record before forwarding. Any other profile is rejected
// rather than activated with controls it cannot run.
const profileCore = "core"

const (
	maxPolicyBytes   = 64 << 10
	maxPolicyDepth   = 16
	maxUpstreams     = 64
	maxAPIs          = 64
	maxOperations    = 256
	maxRoles         = 64
	maxPermits       = 4096
	maxSubjects      = 1024
	maxIDLength      = 64
	maxSubjectLength = 128
	maxTargetLength  = 256
	maxBodyLimit     = 1 << 20
)

// Policy is the complete permanent policy document. Every publication carries
// a whole document; there is no partial update.
type Policy struct {
	SchemaVersion int         `json:"schema_version"`
	Profile       string      `json:"profile"`
	Upstreams     []Upstream  `json:"upstreams"`
	APIs          []API       `json:"apis"`
	Operations    []Operation `json:"operations"`
	Roles         []Role      `json:"roles"`
	Permits       []Permit    `json:"permits"`
	Subjects      []Subject   `json:"subjects"`
}

// Upstream is one entry of the closed destination inventory.
type Upstream struct {
	ID   string `json:"id"`
	Dial string `json:"dial"`
}

// API registers one managed API and the single upstream that serves it.
type API struct {
	ID       string `json:"id"`
	Upstream string `json:"upstream"`
}

// Operation binds a method and target of an API to an access decision and
// the controls that apply to it.
type Operation struct {
	ID      string          `json:"id"`
	API     string          `json:"api"`
	Method  string          `json:"method"`
	Target  string          `json:"target"`
	Access  string          `json:"access"`
	Auth    string          `json:"auth"`
	Body    *BodyRule       `json:"body,omitempty"`
	Headers json.RawMessage `json:"headers,omitempty"` // JSON Schema for declared request headers
	Rate    *RateRule       `json:"rate,omitempty"`
}

// BodyRule declares a JSON request body, its size ceiling and the JSON Schema
// it must satisfy. An operation without a BodyRule accepts no request body.
type BodyRule struct {
	MaxBytes int64           `json:"max_bytes"`
	Schema   json.RawMessage `json:"schema"`
}

// RateRule limits how many requests each verified subject may make to the
// operation within a sliding window.
type RateRule struct {
	Algorithm     string `json:"algorithm"`
	Count         int64  `json:"count"`
	WindowSeconds int64  `json:"window_seconds"`
	Key           string `json:"key"`
}

// Role is a flat role identifier.
type Role struct {
	ID string `json:"id"`
}

// Permit grants a role an operation. Roles have no hierarchy: a permit
// applies to the named role only.
type Permit struct {
	Role      string `json:"role"`
	Operation string `json:"operation"`
}

// Subject is the trusted membership record of one token subject of the
// configured issuer. Its ID is compared with the verified "sub" claim.
type Subject struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Roles  []string `json:"roles"`
}

// PolicyError is a typed candidate rejection. Field is the JSON path of the
// offending value when one applies.
type PolicyError struct {
	Code  string
	Field string
}

func (e *PolicyError) Error() string {
	if e.Field == "" {
		return e.Code
	}
	return e.Code + " at " + e.Field
}

func invalid(code, field string, args ...any) *PolicyError {
	return &PolicyError{Code: code, Field: fmt.Sprintf(field, args...)}
}

var allowedMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// Generation is an immutable compiled effective policy. Requests capture one
// generation and never observe another.
type Generation struct {
	ID       string
	Revision int64
	Profile  string
	// Overrides lists, in order, the temporary overrides applied to the base
	// revision.
	Overrides []int64
	// NotAfter, when set, is the earliest persisted deadline the generation
	// depends on (a provisional base's confirmation deadline or an override's
	// expiry), after which it must no longer admit requests even if the
	// controller has not yet replaced it.
	NotAfter time.Time

	exact    map[string]*target
	prefixes []*target // ordered most specific first
	subjects map[string]*member
}

// member is a compiled subject: whether it may authenticate at all, and its
// roles in sorted order.
type member struct {
	active bool
	roles  []string
}

type target struct {
	pattern string
	prefix  string // "/a/" for "/a/*", "/" for "/*"; empty for exact targets
	methods map[string]*rule
	allow   string // value for the Allow header
}

type rule struct {
	operation string
	api       string
	upstream  string
	dial      string
	permitted bool
	public    bool            // auth "none": no token is read
	roles     map[string]bool // roles holding a permit for this operation
	body      *bodyRule       // nil: the operation accepts no body
	headers   *headerRule     // nil: no header schema
	rate      *rateRule       // nil: no rate limit
}

// DecodePolicy parses a policy document. It rejects oversized or deeply
// nested input, repeated or non-lowercase member names, unknown fields,
// unsupported schema versions and trailing data.
func DecodePolicy(document []byte) (Policy, error) {
	if len(document) > maxPolicyBytes {
		return Policy{}, invalid("LIMIT_EXCEEDED", "")
	}
	if err := checkMembers(document, maxPolicyDepth); err != nil {
		return Policy{}, err
	}

	// Read the version first, so a document written for another version is
	// reported as such rather than as a set of unknown fields.
	var header struct {
		SchemaVersion int `json:"schema_version"`
	}
	if err := json.Unmarshal(document, &header); err != nil {
		return Policy{}, invalid("SCHEMA_INVALID", "")
	}
	if header.SchemaVersion != schemaVersion {
		return Policy{}, invalid("SCHEMA_VERSION_UNSUPPORTED", "schema_version")
	}

	var p Policy
	if err := decodeStrict(document, &p); err != nil {
		if strings.HasPrefix(err.Error(), "json: unknown field ") {
			return Policy{}, invalid("UNKNOWN_FIELD", "")
		}
		if typeErr, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
			return Policy{}, invalid("SCHEMA_INVALID", "%s", typeErr.Field)
		}
		return Policy{}, invalid("SCHEMA_INVALID", "")
	}
	return p, nil
}

// decodeStrict decodes exactly one JSON value, rejecting unknown fields and
// anything after the value.
func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("data after the JSON value")
	}
	return nil
}

// checkMembers rejects JSON nested deeper than maxDepth, and objects with a
// repeated member name or a name outside [a-z0-9_]. encoding/json keeps the
// last of repeated names and matches names case-insensitively, so without
// this check two readers of the same document could disagree about it.
//
// Values of members named "schema" or "headers" are JSON Schemas, whose
// keywords and property names are case-sensitive and not limited to
// [a-z0-9_]. They are decoded as raw JSON, never into Go structs, so only the
// repeated-name and depth rules apply inside them.
func checkMembers(data []byte, maxDepth int) error {
	type container struct {
		object   bool
		wantName bool
		names    map[string]bool
		lastName string
		schema   bool // inside a schema value
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var stack []*container
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return invalid("SCHEMA_INVALID", "")
		}

		var parent *container
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}

		if delim, ok := token.(json.Delim); ok && (delim == '}' || delim == ']') {
			stack = stack[:len(stack)-1]
			continue
		}

		if parent != nil && parent.object && parent.wantName {
			name := token.(string)
			if !parent.schema && !isMemberName(name) {
				return invalid("UNKNOWN_FIELD", "")
			}
			if parent.names[name] {
				return invalid("DUPLICATE_MEMBER", "%s", name)
			}
			parent.names[name] = true
			parent.lastName = name
			parent.wantName = false
			continue
		}

		// A value: once it is complete, an enclosing object expects a name.
		if parent != nil && parent.object {
			parent.wantName = true
		}
		if delim, ok := token.(json.Delim); ok {
			if len(stack) == maxDepth {
				return invalid("LIMIT_EXCEEDED", "")
			}
			inSchema := parent != nil &&
				(parent.schema || (parent.object && (parent.lastName == "schema" || parent.lastName == "headers")))
			stack = append(stack, &container{
				object:   delim == '{',
				wantName: delim == '{',
				names:    map[string]bool{},
				schema:   inSchema,
			})
		}
	}
}

func isMemberName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		lower := c >= 'a' && c <= 'z'
		digit := c >= '0' && c <= '9'
		if !lower && !digit && c != '_' {
			return false
		}
	}
	return true
}

// Compile validates a complete policy and builds its generation. It either
// returns a complete generation or an error; it never returns a partial one.
// It has no side effects. A non-zero notAfter is the earliest persisted
// deadline the generation depends on.
func Compile(p Policy, revision int64, notAfter time.Time) (*Generation, error) {
	if p.SchemaVersion != schemaVersion {
		return nil, invalid("SCHEMA_VERSION_UNSUPPORTED", "schema_version")
	}
	if p.Profile == "" {
		return nil, invalid("INVALID_VALUE", "profile")
	}
	if p.Profile != profileCore {
		return nil, invalid("UNSUPPORTED_FEATURE", "profile")
	}
	if err := checkCounts(p); err != nil {
		return nil, err
	}

	dials := map[string]string{}
	usedDials := map[string]bool{}
	for i, u := range p.Upstreams {
		if err := checkID(u.ID, "upstreams[%d].id", i); err != nil {
			return nil, err
		}
		if _, exists := dials[u.ID]; exists {
			return nil, invalid("DUPLICATE_ID", "upstreams[%d].id", i)
		}
		if !isLoopbackDial(u.Dial) {
			return nil, invalid("INVALID_UPSTREAM", "upstreams[%d].dial", i)
		}
		// Two upstream identities for one destination would make receipts
		// and events ambiguous about which service was reached.
		if usedDials[u.Dial] {
			return nil, invalid("INVALID_UPSTREAM", "upstreams[%d].dial", i)
		}
		dials[u.ID] = u.Dial
		usedDials[u.Dial] = true
	}

	apiUpstream := map[string]string{}
	for i, a := range p.APIs {
		if err := checkID(a.ID, "apis[%d].id", i); err != nil {
			return nil, err
		}
		if _, exists := apiUpstream[a.ID]; exists {
			return nil, invalid("DUPLICATE_ID", "apis[%d].id", i)
		}
		if _, ok := dials[a.Upstream]; !ok {
			return nil, invalid("UNKNOWN_REFERENCE", "apis[%d].upstream", i)
		}
		apiUpstream[a.ID] = a.Upstream
	}

	g := &Generation{Revision: revision, Profile: p.Profile, exact: map[string]*target{}}
	if !notAfter.IsZero() {
		g.NotAfter = notAfter.Round(0)
	}
	targets := map[string]*target{}
	rules := map[string]*rule{}
	for i, op := range p.Operations {
		if err := checkID(op.ID, "operations[%d].id", i); err != nil {
			return nil, err
		}
		if _, exists := rules[op.ID]; exists {
			return nil, invalid("DUPLICATE_ID", "operations[%d].id", i)
		}
		upstream, ok := apiUpstream[op.API]
		if !ok {
			return nil, invalid("UNKNOWN_REFERENCE", "operations[%d].api", i)
		}
		if !allowedMethods[op.Method] {
			return nil, invalid("INVALID_METHOD", "operations[%d].method", i)
		}
		prefix, ok := parseTarget(op.Target)
		if !ok {
			return nil, invalid("INVALID_TARGET", "operations[%d].target", i)
		}
		if op.Access != "allow" && op.Access != "deny" {
			return nil, invalid("INVALID_VALUE", "operations[%d].access", i)
		}
		if op.Auth != "required" && op.Auth != "none" {
			return nil, invalid("INVALID_VALUE", "operations[%d].auth", i)
		}

		t := targets[op.Target]
		if t == nil {
			t = &target{pattern: op.Target, prefix: prefix, methods: map[string]*rule{}}
			targets[op.Target] = t
		}
		if _, exists := t.methods[op.Method]; exists {
			return nil, invalid("DUPLICATE_OPERATION", "operations[%d]", i)
		}
		r := &rule{
			operation: op.ID,
			api:       op.API,
			upstream:  upstream,
			dial:      dials[upstream],
			permitted: op.Access == "allow",
			public:    op.Auth == "none",
			roles:     map[string]bool{},
		}
		if err := compileControls(op, i, r); err != nil {
			return nil, err
		}
		t.methods[op.Method] = r
		rules[op.ID] = r
	}

	roles, err := checkPermits(p, rules)
	if err != nil {
		return nil, err
	}
	g.subjects, err = compileSubjects(p.Subjects, roles)
	if err != nil {
		return nil, err
	}

	for _, t := range targets {
		methods := make([]string, 0, len(t.methods))
		for m := range t.methods {
			methods = append(methods, m)
		}
		sort.Strings(methods)
		t.allow = strings.Join(methods, ", ")
		if t.prefix == "" {
			g.exact[t.pattern] = t
		} else {
			g.prefixes = append(g.prefixes, t)
		}
	}
	// Two prefixes that both match a path differ in length, so ordering by
	// length alone selects the most specific one regardless of declaration
	// order.
	sort.Slice(g.prefixes, func(i, j int) bool { return len(g.prefixes[i].prefix) > len(g.prefixes[j].prefix) })

	g.ID = generationID(p, revision, notAfter, nil)
	return g, nil
}

// denyAll reports whether the policy can admit no request at all: it holds no
// permits and no subjects, so no authenticated request can pass RBAC, and no
// operation is both allowed and public. Only such a policy may become the
// first base of an installation or the base that ends a recovery.
func denyAll(p Policy) bool {
	if len(p.Permits) > 0 || len(p.Subjects) > 0 {
		return false
	}
	for _, op := range p.Operations {
		if op.Access == "allow" && op.Auth == "none" {
			return false
		}
	}
	return true
}

// controlChange names one control and the target it applies to. It is the
// identity by which a candidate's effect on enforcement is reported, and by
// which a temporary override will be compared with a candidate.
type controlChange struct {
	Control string `json:"control"`
	Target  string `json:"target"`
}

// policyChanges lists, in a stable order, every control target whose
// definition differs between two complete policies: an upstream, API,
// operation (route, method, access, authentication, body and header rules),
// its rate rule, a role, a permit (role and operation) or a subject.
func policyChanges(before, after Policy) []controlChange {
	// encode gives a definition a comparable form; policy types always encode.
	encode := func(value any) string {
		data, _ := json.Marshal(value)
		return string(data)
	}
	// collect maps each control to its targets and their definitions.
	collect := func(p Policy) map[string]map[string]string {
		d := map[string]map[string]string{"upstream": {}, "api": {}, "operation": {}, "rate": {}, "role": {}, "permit": {}, "subject": {}}
		for _, u := range p.Upstreams {
			d["upstream"][u.ID] = u.Dial
		}
		for _, a := range p.APIs {
			d["api"][a.ID] = a.Upstream
		}
		for _, op := range p.Operations {
			if op.Rate != nil {
				d["rate"][op.ID] = encode(op.Rate)
			}
			op.Rate = nil
			d["operation"][op.ID] = encode(op)
		}
		for _, r := range p.Roles {
			d["role"][r.ID] = ""
		}
		for _, permit := range p.Permits {
			d["permit"][permit.Role+":"+permit.Operation] = ""
		}
		for _, s := range p.Subjects {
			d["subject"][s.ID] = encode(s)
		}
		return d
	}
	old, current := collect(before), collect(after)

	changes := []controlChange{}
	for control, targets := range current {
		for target, definition := range targets {
			if previous, found := old[control][target]; !found || previous != definition {
				changes = append(changes, controlChange{control, target})
			}
		}
		for target := range old[control] {
			if _, found := targets[target]; !found {
				changes = append(changes, controlChange{control, target})
			}
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Control != changes[j].Control {
			return changes[i].Control < changes[j].Control
		}
		return changes[i].Target < changes[j].Target
	})
	return changes
}

func checkCounts(p Policy) error {
	limits := []struct {
		field string
		count int
		limit int
	}{
		{"upstreams", len(p.Upstreams), maxUpstreams},
		{"apis", len(p.APIs), maxAPIs},
		{"operations", len(p.Operations), maxOperations},
		{"roles", len(p.Roles), maxRoles},
		{"permits", len(p.Permits), maxPermits},
		{"subjects", len(p.Subjects), maxSubjects},
	}
	for _, l := range limits {
		if l.count > l.limit {
			return invalid("LIMIT_EXCEEDED", "%s", l.field)
		}
	}
	return nil
}

// compileControls compiles an operation's body schema, header schema and rate
// rule into its rule. A rule that cannot be compiled rejects the whole policy.
func compileControls(op Operation, i int, r *rule) error {
	var err error
	if op.Body != nil {
		if op.Body.MaxBytes <= 0 || op.Body.MaxBytes > maxBodyLimit {
			return invalid("INVALID_VALUE", "operations[%d].body.max_bytes", i)
		}
		schema, _, err := compileSchema(op.Body.Schema, schemaBody, fmt.Sprintf("operations[%d].body.schema", i))
		if err != nil {
			return err
		}
		r.body = &bodyRule{maxBytes: op.Body.MaxBytes, schema: schema}
	}
	if op.Headers != nil {
		r.headers, err = compileHeaders(op.Headers, fmt.Sprintf("operations[%d].headers", i))
		if err != nil {
			return err
		}
	}
	if op.Rate != nil {
		// The only counting key is the verified subject, so a public operation
		// has no trusted key and cannot be rate limited.
		if r.public {
			return invalid("INVALID_VALUE", "operations[%d].rate", i)
		}
		r.rate, err = compileRate(*op.Rate, fmt.Sprintf("operations[%d].rate", i))
		if err != nil {
			return err
		}
	}
	return nil
}

// checkPermits validates the role declarations and the permits that refer to
// them, and records each permit on its operation's rule. Unknown roles and
// operations are rejected, so a reference can never grant anything by
// accident. It returns the declared roles.
func checkPermits(p Policy, rules map[string]*rule) (map[string]bool, error) {
	roles := map[string]bool{}
	for i, r := range p.Roles {
		if err := checkID(r.ID, "roles[%d].id", i); err != nil {
			return nil, err
		}
		if roles[r.ID] {
			return nil, invalid("DUPLICATE_ID", "roles[%d].id", i)
		}
		roles[r.ID] = true
	}

	for i, permit := range p.Permits {
		if !roles[permit.Role] {
			return nil, invalid("UNKNOWN_REFERENCE", "permits[%d].role", i)
		}
		r, ok := rules[permit.Operation]
		if !ok {
			return nil, invalid("UNKNOWN_REFERENCE", "permits[%d].operation", i)
		}
		// A permit on an operation that needs no authentication could never
		// apply, so the two declarations contradict each other.
		if r.public {
			return nil, invalid("INVALID_VALUE", "permits[%d].operation", i)
		}
		if r.roles[permit.Role] {
			return nil, invalid("DUPLICATE_ID", "permits[%d]", i)
		}
		r.roles[permit.Role] = true
	}
	return roles, nil
}

// compileSubjects validates the membership records against the declared
// roles. A subject is never created from a token: only these records exist.
func compileSubjects(subjects []Subject, roles map[string]bool) (map[string]*member, error) {
	members := map[string]*member{}
	for i, s := range subjects {
		if !isSubjectID(s.ID) {
			return nil, invalid("INVALID_VALUE", "subjects[%d].id", i)
		}
		if _, exists := members[s.ID]; exists {
			return nil, invalid("DUPLICATE_ID", "subjects[%d].id", i)
		}
		if s.Status != "active" && s.Status != "disabled" {
			return nil, invalid("INVALID_VALUE", "subjects[%d].status", i)
		}
		if len(s.Roles) > maxRoles {
			return nil, invalid("LIMIT_EXCEEDED", "subjects[%d].roles", i)
		}

		m := &member{active: s.Status == "active"}
		held := map[string]bool{}
		for j, role := range s.Roles {
			if !roles[role] {
				return nil, invalid("UNKNOWN_REFERENCE", "subjects[%d].roles[%d]", i, j)
			}
			if held[role] {
				return nil, invalid("DUPLICATE_ID", "subjects[%d].roles[%d]", i, j)
			}
			held[role] = true
			m.roles = append(m.roles, role)
		}
		sort.Strings(m.roles)
		members[s.ID] = m
	}
	return members, nil
}

// isSubjectID accepts 1 to 128 printable ASCII characters without spaces. The
// same rule applies to the "sub" claim, so a subject identifier is always
// safe to log and to forward in a header.
func isSubjectID(id string) bool {
	if id == "" || len(id) > maxSubjectLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7e {
			return false
		}
	}
	return true
}

// checkID accepts short identifiers made of letters, digits, '.', '_' and
// '-', starting with a letter or digit, so identifiers are safe to log and to
// compare byte for byte.
func checkID(id, field string, args ...any) error {
	if id == "" || len(id) > maxIDLength {
		return invalid("INVALID_VALUE", field, args...)
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		alphanumeric := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		if alphanumeric {
			continue
		}
		if i == 0 || (c != '.' && c != '_' && c != '-') {
			return invalid("INVALID_VALUE", field, args...)
		}
	}
	return nil
}

// generationID identifies a generation by its base revision, the overrides
// applied to it (as "+o<id>.<id>…") and a digest of the compiled document and
// deadline. A generation without overrides keeps the form "r<revision>-…".
func generationID(p Policy, revision int64, notAfter time.Time, overrides []int64) string {
	document, _ := json.Marshal(p)
	sum := sha256.New()
	fmt.Fprintf(sum, "%s|%d|%d|", compilerVersion, revision, notAfter.UnixMilli())
	var label strings.Builder
	fmt.Fprintf(&label, "r%d", revision)
	for i, id := range overrides {
		separator := "."
		if i == 0 {
			separator = "+o"
		}
		fmt.Fprintf(&label, "%s%d", separator, id)
		fmt.Fprintf(sum, "o%d|", id)
	}
	sum.Write(document)
	return fmt.Sprintf("%s-%s", label.String(), hex.EncodeToString(sum.Sum(nil))[:12])
}

// isLoopbackDial accepts only literal loopback addresses, so a destination can
// neither be resolved through DNS nor expanded from request placeholders.
func isLoopbackDial(dial string) bool {
	addr, err := netip.ParseAddrPort(dial)
	return err == nil && addr.Port() != 0 && addr.Addr().IsLoopback()
}

// parseTarget returns the match prefix for "/a/*" targets, or "" for exact
// targets. Targets must already be canonical.
func parseTarget(pattern string) (string, bool) {
	if len(pattern) > maxTargetLength {
		return "", false
	}
	path, isPrefix := strings.CutSuffix(pattern, "/*")
	if isPrefix {
		path += "/"
	}
	if !isCanonical(path) || strings.Contains(path, "*") {
		return "", false
	}
	if isPrefix {
		return path, true
	}
	return "", true
}

// CanonicalPath returns the single path interpretation used for matching and
// forwarding. Any encoding whose meaning could differ between the gateway and
// an upstream is rejected rather than normalised.
func CanonicalPath(r *http.Request) (string, bool) {
	escaped := strings.ToLower(r.URL.EscapedPath())
	for _, banned := range []string{"%2f", "%5c", "%2e", "%00"} {
		if strings.Contains(escaped, banned) {
			return "", false
		}
	}
	path := r.URL.Path
	if !isCanonical(path) {
		return "", false
	}
	return path, true
}

func isCanonical(path string) bool {
	if !strings.HasPrefix(path, "/") {
		return false
	}
	for i := 0; i < len(path); i++ {
		c := path[i]
		if c < 0x21 || c > 0x7e || c == '\\' || c == '%' {
			return false
		}
	}
	segments := strings.Split(path[1:], "/")
	for i, segment := range segments {
		if segment == "." || segment == ".." {
			return false
		}
		// Only a single trailing empty segment (a trailing slash) is allowed.
		if segment == "" && i != len(segments)-1 {
			return false
		}
	}
	return true
}

// expired reports whether the generation's earliest deadline has passed at
// now, a reading of trustedNow.
func (g *Generation) expired(now time.Time) bool {
	return !g.NotAfter.IsZero() && !now.Before(g.NotAfter)
}

// processStart anchors this process's monotonic clock to the wall clock once,
// at startup. It is never changed.
var processStart = time.Now()

// wallLead is the furthest, in nanoseconds, that the wall clock has been ahead
// of the monotonic estimate when a deadline was set in this process. It only
// grows.
var wallLead atomic.Int64

// trustedNow is the time against which every persisted deadline is decided
// and every controller timestamp is taken; see trustedTime.
func trustedNow() time.Time {
	return trustedTime(time.Now().Round(0).Add(wallOffset()), time.Since(processStart))
}

// deadlineNow is trustedNow for setting a deadline: a provisional base's or an
// override's. It first keeps any lead the wall clock has over the monotonic
// estimate, so a deadline set from a wall clock that is ahead is still reached
// after its own period if that clock is later set back. The cost is that, once
// the lead is kept, later timestamps stay that far ahead until a restart, and
// that restart waits for real time to pass them (the high-water check); the
// deadline's own record already carries the lead. Other readings, including
// every request, never raise it, so a brief forward step with no new deadline
// leaves no trace.
func deadlineNow() time.Time {
	wall := time.Now().Round(0).Add(wallOffset())
	elapsed := time.Since(processStart)
	lead := int64(wall.Sub(processStart.Round(0).Add(elapsed)))
	for {
		seen := wallLead.Load()
		if lead <= seen || wallLead.CompareAndSwap(seen, lead) {
			break
		}
	}
	return trustedTime(wall, elapsed)
}

// trustedTime returns the later of the wall clock reading wall and the wall
// time the monotonic clock implies after elapsed since processStart, advanced
// by the wall clock's largest lead when a deadline was set. A deadline has
// passed once either clock reaches it: a wall clock set back cannot extend it,
// and neither can a monotonic clock that stood still across a system suspend.
// A wall clock set forward ends deadlines early, which is the safe direction.
// The monotonic estimate is anchored at process start, not at each
// publication, so republishing after a wall clock change cannot move a
// deadline either.
func trustedTime(wall time.Time, elapsed time.Duration) time.Time {
	estimate := processStart.Round(0).Add(elapsed + time.Duration(wallLead.Load()))
	if wall.After(estimate) {
		return wall
	}
	return estimate
}

// match selects the most specific target for a canonical path, then the
// method on that target only. A less specific target is never consulted for a
// missing method.
func (g *Generation) match(path, method string) (*target, *rule) {
	t := g.exact[path]
	if t == nil {
		for _, candidate := range g.prefixes {
			rest, ok := strings.CutPrefix(path, candidate.prefix)
			if ok && rest != "" {
				t = candidate
				break
			}
		}
	}
	if t == nil {
		return nil, nil
	}
	return t, t.methods[method]
}

// authorise resolves a verified subject to its trusted roles in this
// generation and allows the operation when at least one of those roles holds
// a permit for it. It returns the subject's roles, or the refusal reason.
func (g *Generation) authorise(subject string, r *rule) ([]string, string) {
	m := g.subjects[subject]
	if m == nil {
		return nil, "unknown_subject"
	}
	if !m.active {
		return nil, "disabled_subject"
	}
	for _, role := range m.roles {
		if r.roles[role] {
			return m.roles, ""
		}
	}
	return nil, "no_permit"
}
