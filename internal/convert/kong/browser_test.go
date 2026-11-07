// Browser access and allow list rows: Kong's cors and ip-restriction
// plugins become the API's settings, and the writer emits them again.

package kong

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Alex-Zechariah-02/apiward/internal/exchange"
	"github.com/Alex-Zechariah-02/apiward/internal/policy"
)

func TestKongBrowser(t *testing.T) {
	result := read(t, "browser.yaml")
	settings := func(document map[string]any) string {
		data, _ := json.Marshal([]any{document[policy.CORSExtension], document[policy.AddressesExtension]})
		return string(data)
	}
	want := `[{"credentials":false,"expose":["X-Total"],"headers":["Content-Type"],"max_age_seconds":600,"methods":["GET","POST"],` +
		`"origins":["https://shop.example"]},["10.0.0.0/8","192.0.2.7/32"]]`
	if got := settings(result.Document); got != want {
		t.Fatalf("settings %s\nwant %s", got, want)
	}
	classes := map[string]exchange.Class{}
	for _, item := range result.Ledger.Items {
		classes[strings.TrimPrefix(item.ID, Format+":")] = item.Class
	}
	if classes["plugin:service.site:cors"] != exchange.Equivalent || classes["operation:pages:GET /pages:ip-restriction"] != exchange.Equivalent {
		t.Fatalf("items %v", classes)
	}

	// From an exchange file of another source, every route is written from
	// its operation, with the API's cors and ip-restriction plugins; read
	// again, they give the same settings.
	result.Ledger.Source.Format, result.Ledger.Carried = "openapi", nil
	out, _, err := Write(result.Document, result.Ledger, Options{})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(out)
	var root map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&root); err != nil {
		t.Fatal(err)
	}
	again, err := Read(root, data)
	if err != nil {
		t.Fatal(err)
	}
	if got := settings(again.Document); got != want {
		t.Fatalf("written and read again: %s\nwant %s", got, want)
	}
}
