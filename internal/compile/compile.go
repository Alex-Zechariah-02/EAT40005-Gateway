// Compiling a policy document into an immutable generation.

package compile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// compilerVersion is part of every generation identity so that a change in
// compilation rules is visible even when the policy revision is unchanged.
const compilerVersion = "5"

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
	roles    map[string]bool
}

type target struct {
	pattern string
	prefix  string // "/a/" for "/a/*", "/" for "/*"; empty for exact targets
	methods map[string]*Rule
	allow   string // value for the Allow header
}

type Rule struct {
	operation string
	api       string
	upstream  string
	dial      string
	permitted bool
	public    bool            // auth "none": no token is read
	roles     map[string]bool // roles holding a permit for this operation
	body      *BodyRule       // nil: the operation accepts no body
	headers   *headerRule     // nil: no header schema
	rate      *RateRule       // nil: no rate limit
	rateKey   string          // subject or direct peer address, checked by the compiler
}

// Compile validates a complete policy and builds its generation. It either
// returns a complete generation or an error; it never returns a partial one.
// It has no side effects. A non-zero notAfter is the earliest persisted
// deadline the generation depends on.
func Compile(p policy.Policy, revision int64, notAfter time.Time) (*Generation, error) {
	if p.SchemaVersion != policy.SchemaVersion {
		return nil, policy.Invalid("SCHEMA_VERSION_UNSUPPORTED", "schema_version")
	}
	if p.Profile == "" {
		return nil, policy.Invalid("INVALID_VALUE", "profile")
	}
	if p.Profile != policy.ProfileCore {
		return nil, policy.Invalid("UNSUPPORTED_FEATURE", "profile")
	}
	if err := checkCounts(p); err != nil {
		return nil, err
	}

	dials := map[string]string{}
	usedDials := map[string]bool{}
	for i, u := range p.Upstreams {
		if err := policy.CheckID(u.ID, "upstreams[%d].id", i); err != nil {
			return nil, err
		}
		if _, exists := dials[u.ID]; exists {
			return nil, policy.Invalid("DUPLICATE_ID", "upstreams[%d].id", i)
		}
		if !IsLoopbackDial(u.Dial) {
			return nil, policy.Invalid("INVALID_UPSTREAM", "upstreams[%d].dial", i)
		}
		// Two upstream identities for one destination would make receipts
		// and events ambiguous about which service was reached.
		if usedDials[u.Dial] {
			return nil, policy.Invalid("INVALID_UPSTREAM", "upstreams[%d].dial", i)
		}
		dials[u.ID] = u.Dial
		usedDials[u.Dial] = true
	}

	apiUpstream := map[string]string{}
	for i, a := range p.APIs {
		if err := policy.CheckID(a.ID, "apis[%d].id", i); err != nil {
			return nil, err
		}
		if _, exists := apiUpstream[a.ID]; exists {
			return nil, policy.Invalid("DUPLICATE_ID", "apis[%d].id", i)
		}
		if _, ok := dials[a.Upstream]; !ok {
			return nil, policy.Invalid("UNKNOWN_REFERENCE", "apis[%d].upstream", i)
		}
		apiUpstream[a.ID] = a.Upstream
	}

	g := &Generation{Revision: revision, Profile: p.Profile, exact: map[string]*target{}}
	if !notAfter.IsZero() {
		g.NotAfter = notAfter.Round(0)
	}
	targets := map[string]*target{}
	rules := map[string]*Rule{}
	for i, op := range p.Operations {
		if err := policy.CheckID(op.ID, "operations[%d].id", i); err != nil {
			return nil, err
		}
		if _, exists := rules[op.ID]; exists {
			return nil, policy.Invalid("DUPLICATE_ID", "operations[%d].id", i)
		}
		upstream, ok := apiUpstream[op.API]
		if !ok {
			return nil, policy.Invalid("UNKNOWN_REFERENCE", "operations[%d].api", i)
		}
		if !policy.AllowedMethods[op.Method] {
			return nil, policy.Invalid("INVALID_METHOD", "operations[%d].method", i)
		}
		prefix, ok := parseTarget(op.Target)
		if !ok {
			return nil, policy.Invalid("INVALID_TARGET", "operations[%d].target", i)
		}
		if op.Access != "allow" && op.Access != "deny" {
			return nil, policy.Invalid("INVALID_VALUE", "operations[%d].access", i)
		}
		if op.Auth != "required" && op.Auth != "none" {
			return nil, policy.Invalid("INVALID_VALUE", "operations[%d].auth", i)
		}

		t := targets[op.Target]
		if t == nil {
			t = &target{pattern: op.Target, prefix: prefix, methods: map[string]*Rule{}}
			targets[op.Target] = t
		}
		if _, exists := t.methods[op.Method]; exists {
			return nil, policy.Invalid("DUPLICATE_OPERATION", "operations[%d]", i)
		}
		r := &Rule{
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
	g.roles = roles

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

	g.ID = GenerationID(p, revision, notAfter, nil)
	return g, nil
}

// DenyAll reports whether the policy can admit no request at all: it holds no
// permits, so no authenticated request can pass RBAC, and no
// operation is both allowed and public. Only such a policy may become the
// first base of an installation or the base that ends a recovery.
func DenyAll(p policy.Policy) bool {
	if len(p.Permits) > 0 {
		return false
	}
	for _, op := range p.Operations {
		if op.Access == "allow" && op.Auth == "none" {
			return false
		}
	}
	return true
}

// compileControls compiles an operation's body schema, header schema and rate
// rule into its rule. A rule that cannot be compiled rejects the whole policy.
func compileControls(op policy.Operation, i int, r *Rule) error {
	var err error
	if op.Body != nil {
		if op.Body.MaxBytes <= 0 || op.Body.MaxBytes > policy.MaxBodyLimit {
			return policy.Invalid("INVALID_VALUE", "operations[%d].body.max_bytes", i)
		}
		schema, _, err := CompileSchema(op.Body.Schema, SchemaBody, fmt.Sprintf("operations[%d].body.schema", i))
		if err != nil {
			return err
		}
		r.body = &BodyRule{maxBytes: op.Body.MaxBytes, schema: schema}
	}
	if op.Headers != nil {
		r.headers, err = compileHeaders(op.Headers, fmt.Sprintf("operations[%d].headers", i))
		if err != nil {
			return err
		}
	}
	if op.Rate != nil {
		if r.public && op.Rate.Key == RateKeySubject {
			return policy.Invalid("INVALID_VALUE", "operations[%d].rate", i)
		}
		r.rate, err = CompileRate(*op.Rate, fmt.Sprintf("operations[%d].rate", i))
		if err != nil {
			return err
		}
		r.rateKey = op.Rate.Key
	}
	return nil
}

// GenerationID identifies a generation by its base revision, the overrides
// applied to it (as "+o<id>.<id>…") and a digest of the compiled document and
// deadline. A generation without overrides keeps the form "r<revision>-…".
func GenerationID(p policy.Policy, revision int64, notAfter time.Time, overrides []int64) string {
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
