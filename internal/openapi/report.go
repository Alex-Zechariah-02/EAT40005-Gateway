// The import report: what was mapped, changed, assumed or refused, and the exit status it implies.

package openapi

// Exit statuses of the import command.
const (
	ExitComplete = 0 // everything mapped
	ExitFailed   = 1 // nothing written
	ExitPartial  = 2 // written, but some operations were refused or widened
)

// Refusal codes for one operation.
const (
	CodeMethod          = "METHOD_REFUSED"
	CodeOperationID     = "OPERATION_ID_INVALID"
	CodeDuplicateID     = "OPERATION_ID_DUPLICATE"
	CodePathTemplate    = "PATH_TEMPLATE_UNSUPPORTED"
	CodeTargetCollision = "TARGET_COLLISION"
	CodeSecurity        = "SECURITY_SCHEME_UNSUPPORTED"
	CodeMediaType       = "MEDIA_TYPE_UNSUPPORTED"
	CodeCycle           = "SCHEMA_CYCLE"
	CodeExternalRef     = "EXTERNAL_REF"
	CodeRefInvalid      = "REF_INVALID"
	CodeKeyword         = "SCHEMA_KEYWORD_UNSUPPORTED"
	CodeSchemaLimit     = "SCHEMA_LIMIT_EXCEEDED"
	CodeCompiler        = "COMPILER_REFUSED"
)

// Report is the stable, documented account of one import. Every list is
// present, never null, and in a deterministic order.
type Report struct {
	SpecVersion        string        `json:"spec_version"`
	SourceFormat       Format        `json:"source_format"`
	API                string        `json:"api"`
	OperationsInSpec   int           `json:"operations_in_spec"`
	Mapped             []Mapped      `json:"mapped"`
	Widened            []Mapped      `json:"widened"`
	Assumed            []Assumed     `json:"assumed"`
	Changed            []Changed     `json:"changed"`
	Refused            []Refused     `json:"refused"`
	NeedsPermissions   []string      `json:"needs_permissions"`
	ScopesNotMapped    []string      `json:"scopes_not_mapped"`
	NotEnforced        []NotEnforced `json:"not_enforced"`
	AnnotationsRemoved []Removed     `json:"annotations_removed"`
	RemovedOperations  []string      `json:"removed_operations"`
	RemovedPermits     []Permit      `json:"removed_permits"`
	Ignored            []string      `json:"ignored"`
}

// Mapped is an imported operation; Note explains a default it relies on.
type Mapped struct {
	Method    string `json:"method"`
	Path      string `json:"path"`
	Operation string `json:"operation"`
	Target    string `json:"target"`
	Auth      string `json:"auth"`
	Note      string `json:"note,omitempty"`
}

// Assumed is a value the description does not state and the import chose.
type Assumed struct {
	Operation string `json:"operation"`
	Field     string `json:"field"`
	Value     any    `json:"value"`
	Note      string `json:"note,omitempty"`
}

// Changed is behaviour the gateway enforces differently from the description.
type Changed struct {
	Operation string `json:"operation"`
	Field     string `json:"field"`
	Note      string `json:"note"`
}

// Refused is an operation left out of the output, with the reason.
type Refused struct {
	Method     string `json:"method"`
	Path       string `json:"path"`
	Code       string `json:"code"`
	Message    string `json:"message"`
	Field      string `json:"field,omitempty"`
	Suggestion string `json:"suggestion,omitempty"`
}

// NotEnforced is a parameter the gateway does not validate.
type NotEnforced struct {
	Operation string `json:"operation"`
	Parameter string `json:"parameter"`
	In        string `json:"in"`
}

// Removed names a schema annotation dropped from an operation.
type Removed struct {
	Operation string `json:"operation"`
	Field     string `json:"field"`
}

// Permit is a base permit removed because its operation was replaced.
type Permit struct {
	Role      string `json:"role"`
	Operation string `json:"operation"`
}

// newReport returns a report whose lists are all empty rather than null.
func newReport(format Format, api string) *Report {
	return &Report{
		SourceFormat: format, API: api,
		Mapped: []Mapped{}, Widened: []Mapped{}, Assumed: []Assumed{}, Changed: []Changed{},
		Refused: []Refused{}, NeedsPermissions: []string{}, ScopesNotMapped: []string{},
		NotEnforced: []NotEnforced{}, AnnotationsRemoved: []Removed{}, RemovedOperations: []string{},
		RemovedPermits: []Permit{}, Ignored: []string{},
	}
}

// Exit is the command's status for a written result: partial when any
// operation was refused or widened, so a script cannot miss either.
func (r *Report) Exit() int {
	if len(r.Refused) > 0 || len(r.Widened) > 0 {
		return ExitPartial
	}
	return ExitComplete
}
