// The migrate import and policy commands: flags, bounded reads, and outputs written together or not at all.

package migrate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
	"github.com/Alex-Zechariah-02/apiward/internal/overlay"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// MaxInputBytes is the largest configuration or exchange file read.
const MaxInputBytes = 8 << 20

// Synopses of the commands.
const (
	importUsage = `gateway migrate import --from openapi|kong-deck --in <file.json|.yaml|.yml>
  --out <new exchange file.json|.yaml|.yml> --report <new file> [--decisions <overlay>]`
	policyUsage = `gateway migrate policy --exchange <file> --base <policy.json> --api <id> --upstream <id>
  --out <new file> --report <new file> [--decisions <overlay>] [--with-permits]`
)

var commands = map[string]openapi.Subcommand{
	"import": {Usage: importUsage, Required: []string{"from", "in", "out", "report"}, Optional: noDecisions(), Run: runImport},
	"export": {Usage: exportUsage, Required: []string{"to", "exchange", "out", "report"}, Optional: noDecisions(),
		Switches: []string{"with-upstream", "with-trust", "with-blocked"}, Run: runExport},
	"inventory": {Usage: inventoryUsage, Required: []string{"from", "in", "report"}, Run: runInventory},
	"policy": {Usage: policyUsage, Required: []string{"exchange", "base", "api", "upstream", "out", "report"},
		Optional: noDecisions(), Switches: []string{"with-permits"}, Run: runPolicy},
}

// Command runs "migrate <subcommand>" and returns the exit status.
func Command(args []string, stderr io.Writer) int {
	return openapi.Run(commands, args, stderr)
}

// Handles reports whether name is a migrate subcommand, so "migrate
// --config" stays the store upgrade.
func Handles(name string) bool {
	_, known := commands[name]
	return known
}

func runImport(values map[string]string, stderr io.Writer) (int, error) {
	read, known := readers[values["from"]]
	if !known {
		return openapi.ExitFailed, fmt.Errorf("%w: unknown format %q (supported: openapi, kong-deck)", openapi.ErrUsage, policy.Clip(values["from"]))
	}
	outFormat, err := openapi.FormatOf(values["out"])
	if err != nil {
		return openapi.ExitFailed, err
	}
	data, root, format, err := readDocument(values["in"], "in")
	if err != nil {
		return openapi.ExitFailed, err
	}
	source, ledger, err := read(root, data)
	if err != nil {
		return openapi.ExitFailed, err
	}
	classified, err := openapi.Classify(source, format)
	if err != nil {
		return openapi.ExitFailed, err
	}
	if ledger == nil {
		ledger = openapiLedger(source, classified, data)
	}
	document, ledger, stale, err := decidedImport(source, ledger, values["decisions"])
	if err != nil {
		return openapi.ExitFailed, err
	}
	encoded, err := openapi.Encode(document, outFormat)
	if err != nil {
		return openapi.ExitFailed, err
	}
	if _, err := openapi.Decode(encoded, outFormat, MaxInputBytes); err != nil {
		return openapi.ExitFailed, fmt.Errorf("the exchange file would not be readable: %w", err)
	}
	report := newReport(classified, ledger)
	report.StaleDecisions = stale
	if err := openapi.Write(values["out"], json.RawMessage(encoded), values["report"], report); err != nil {
		return openapi.ExitFailed, err
	}
	fmt.Fprintf(stderr, "classified %d settings (%s); see %s\n", len(ledger.Items), report.LedgerSummary, values["report"])
	return report.Exit(), nil
}

func runPolicy(values map[string]string, stderr io.Writer) (int, error) {
	_, root, format, err := readDocument(values["exchange"], "exchange")
	if err != nil {
		return openapi.ExitFailed, err
	}
	root, stale, err := decide(root, values["decisions"])
	if err != nil {
		return openapi.ExitFailed, err
	}
	document, ledger, err := exchange.Split(root)
	if err != nil {
		return openapi.ExitFailed, err
	}
	if err := oneUpstream(ledger); err != nil {
		return openapi.ExitFailed, err
	}
	document, left, err := withoutLooser(document, ledger)
	if err != nil {
		return openapi.ExitFailed, err
	}
	base, err := openapi.ReadPolicy(values["base"], "base")
	if err != nil {
		return openapi.ExitFailed, err
	}
	result, imported, err := openapi.ImportDocument(document, format, openapi.Options{API: values["api"], Upstream: values["upstream"],
		Base: base, MaxBodyBytes: openapi.DefaultBodyBytes, WithPermits: values["with-permits"] == "true"})
	if err != nil {
		return openapi.ExitFailed, limitError(err)
	}
	imported.Refused = append(imported.Refused, left...)
	report := newReport(imported, ledger)
	report.StaleDecisions = stale
	if err := openapi.Write(values["out"], result, values["report"], report); err != nil {
		return openapi.ExitFailed, err
	}
	fmt.Fprintf(stderr, "imported %d operations (%d widened, %d refused); see %s\n",
		len(imported.Mapped)+len(imported.Widened), len(imported.Widened), len(imported.Refused), values["report"])
	return report.Exit(), nil
}

// noDecisions gives --decisions its default: no decisions file.
func noDecisions() map[string]string { return map[string]string{"decisions": ""} }

// decidedImport attaches the ledger and applies the decisions to the
// exchange file before it is written, so the file records them; the result
// passes the ledger's check again.
func decidedImport(root map[string]any, ledger *exchange.Ledger, decisions string) (map[string]any, *exchange.Ledger, []string, error) {
	document, err := exchange.Attach(root, ledger)
	if err != nil || decisions == "" {
		return document, ledger, []string{}, err
	}
	encoded, err := openapi.Encode(document, openapi.JSON)
	if err != nil {
		return nil, nil, nil, err
	}
	value, err := openapi.Decode(encoded, openapi.JSON, MaxInputBytes)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the exchange file would not be readable: %w", err)
	}
	decided, stale, err := decide(value.(map[string]any), decisions)
	if err != nil {
		return nil, nil, nil, err
	}
	if _, ledger, err = exchange.Split(decided); err != nil {
		return nil, nil, nil, err
	}
	return decided, ledger, stale, nil
}

// decide applies the decisions file at path, if one is named, to an
// exchange file and returns it with the stale actions, clipped for the report.
func decide(root map[string]any, path string) (map[string]any, []string, error) {
	if path == "" {
		return root, []string{}, nil
	}
	_, file, _, err := readDocument(path, "decisions")
	if err != nil {
		return nil, nil, err
	}
	decisions, err := overlay.Parse(file)
	if err != nil {
		return nil, nil, fmt.Errorf("decisions: %w", err)
	}
	decided, stale, err := decisions.Apply(root, openapi.DefaultBodyBytes)
	if err != nil {
		return nil, nil, fmt.Errorf("decisions: %w", err)
	}
	for i := range stale {
		stale[i] = policy.Clip(stale[i])
	}
	return decided, stale, nil
}

// readDocument reads a bounded JSON or YAML file through the import's gate
// and returns its bytes, its root object and its format.
func readDocument(path, name string) ([]byte, map[string]any, openapi.Format, error) {
	format, err := openapi.FormatOf(path)
	if err != nil {
		return nil, nil, "", err
	}
	data, err := openapi.ReadLimited(path, MaxInputBytes, name)
	if err != nil {
		return nil, nil, "", err
	}
	value, err := openapi.Decode(data, format, MaxInputBytes)
	if err != nil {
		return nil, nil, "", err
	}
	root, isObject := value.(map[string]any)
	if !isObject {
		return nil, nil, "", errors.New("the " + name + " file is not a JSON object")
	}
	return data, root, format, nil
}
