// Source readers, the migrate export and inventory commands, and the one-service rule of migrate policy.

package migrate

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/Alex-Zechariah-02/apiward/internal/convert/kong"
	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/openapi"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

// Synopses of the export and inventory commands.
const (
	exportUsage = `gateway migrate export --to kong-deck --exchange <file> --out <new file.json|.yaml|.yml>
  --report <new file> [--decisions <overlay>] [--with-upstream] [--with-trust] [--with-blocked]`
	inventoryUsage = `gateway migrate inventory --from kong-deck --in <file.json|.yaml|.yml> --report <new file>`
)

// readers turn a decoded source file into the exchange document and its
// ledger; a nil ledger means the document is the source itself (OpenAPI),
// classified by the import.
var readers = map[string]func(map[string]any, []byte) (map[string]any, *exchange.Ledger, error){
	"openapi": func(root map[string]any, _ []byte) (map[string]any, *exchange.Ledger, error) { return root, nil, nil },
	kong.Format: func(root map[string]any, data []byte) (map[string]any, *exchange.Ledger, error) {
		result, err := kong.Read(root, data)
		if err != nil {
			return nil, nil, err
		}
		return result.Document, result.Ledger, nil
	},
}

// exportReport is the writer's report with the decisions that matched nothing.
type exportReport struct {
	*kong.ExportReport
	StaleDecisions []string `json:"stale_decisions"`
}

func runExport(values map[string]string, stderr io.Writer) (int, error) {
	if values["to"] != kong.Format {
		return openapi.ExitFailed, fmt.Errorf("%w: unknown format %q (supported: kong-deck)", openapi.ErrUsage, policy.Clip(values["to"]))
	}
	outFormat, err := openapi.FormatOf(values["out"])
	if err != nil {
		return openapi.ExitFailed, err
	}
	_, root, _, err := readDocument(values["exchange"], "exchange")
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
	deck, written, err := kong.Write(document, ledger, kong.Options{WithUpstream: values["with-upstream"] == "true",
		WithTrust: values["with-trust"] == "true", WithBlocked: values["with-blocked"] == "true"})
	if err != nil {
		return openapi.ExitFailed, err
	}
	encoded, err := openapi.Encode(deck, outFormat)
	if err != nil {
		return openapi.ExitFailed, err
	}
	report := exportReport{ExportReport: written, StaleDecisions: stale}
	if err := openapi.Write(values["out"], json.RawMessage(encoded), values["report"], report); err != nil {
		return openapi.ExitFailed, err
	}
	fmt.Fprintf(stderr, "wrote %d routes as read and %d from operations; see %s\n", len(written.Restored), len(written.Generated), values["report"])
	if len(stale) > 0 {
		return openapi.ExitPartial, nil
	}
	return written.Exit(), nil
}

func runInventory(values map[string]string, stderr io.Writer) (int, error) {
	if values["from"] != kong.Format {
		return openapi.ExitFailed, fmt.Errorf("%w: unknown format %q (supported: kong-deck)", openapi.ErrUsage, policy.Clip(values["from"]))
	}
	data, root, _, err := readDocument(values["in"], "in")
	if err != nil {
		return openapi.ExitFailed, err
	}
	inventory, err := kong.Take(root, data)
	if err != nil {
		return openapi.ExitFailed, err
	}
	if err := openapi.WriteOne(values["report"], inventory); err != nil {
		return openapi.ExitFailed, err
	}
	fmt.Fprintf(stderr, "%d services, %d routes, %d unsupported; see %s\n", inventory.Services, len(inventory.Routes),
		len(inventory.Unsupported), values["report"])
	if len(inventory.Unsupported) > 0 || len(inventory.Credentials) > 0 {
		return openapi.ExitPartial, nil
	}
	return openapi.ExitComplete, nil
}

// oneUpstream refuses an exchange file whose operations come from more than
// one source service: one API has one upstream, so binding them all to
// --upstream would send requests to the wrong service.
func oneUpstream(ledger *exchange.Ledger) error {
	var services []string
	for _, item := range ledger.Items {
		if item.Facet == "upstream" && item.Class == exchange.Equivalent {
			services = append(services, strings.TrimPrefix(item.ID, ledger.Source.Format+":service:"))
		}
	}
	if len(services) > 1 {
		return fmt.Errorf("the exchange file holds %d services (%s); one API has one upstream: import a file with one service",
			len(services), policy.Clip(strings.Join(services, ", ")))
	}
	return nil
}
