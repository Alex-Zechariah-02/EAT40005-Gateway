// Package openapi turns an OpenAPI 3.0 or 3.1 description, in JSON or YAML,
// into a candidate policy document and a report of what it mapped, assumed,
// changed and refused. It runs offline: it reads only the files it is named,
// never fetches a reference, and never activates anything; the result is
// staged and trialled like any other draft.
//
// The description is untrusted input. It is size- and depth-bounded before
// mapping, YAML constructs JSON cannot express are refused, and every
// operation is checked by the real compiler before it is accepted, so the
// import never holds a second copy of the compiler's rules. It imports only
// policy and compile.
package openapi
