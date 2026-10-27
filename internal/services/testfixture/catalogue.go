package testfixture

import (
	"bytes"
	_ "embed"
	"encoding/json"
)

// catalogueJSON is the expected behaviour of the three fixtures: their
// operations, the roles each operation is intended for, the controlled
// subjects and their roles, and an ordered scenario sequence with the subject
// and expected fixture outcome of each step. It is written independently of
// the gateway and is never derived from observed gateway responses.
//
//go:embed catalogue.json
var catalogueJSON []byte

// Catalogue is the decoded truth catalogue.
type Catalogue struct {
	Version  string   `json:"version"`
	Roles    []string `json:"roles"`
	Fixtures []struct {
		Service    string `json:"service"`
		Version    string `json:"version"`
		SeedSHA256 string `json:"seed_sha256"`
	} `json:"fixtures"`
	Operations []struct {
		ID     string   `json:"id"`
		API    string   `json:"api"`
		Method string   `json:"method"`
		Target string   `json:"target"`
		Auth   string   `json:"auth"`
		Body   bool     `json:"body"`
		Roles  []string `json:"roles"`
	} `json:"operations"`
	Subjects  []Subject  `json:"subjects"`
	Scenarios []Scenario `json:"scenarios"`
}

// Subject is a controlled caller identity and the roles it is meant to hold.
// A disabled subject must be refused whatever its roles.
type Subject struct {
	ID     string   `json:"id"`
	Status string   `json:"status"`
	Roles  []string `json:"roles"`
}

// Scenario is one step of the replay sequence. Steps depend on the state left
// by earlier steps, so they run in order from the seed. Subject is the caller,
// empty for a public operation. Status and the fields after it are the
// fixture's own outcome. GatewayStage, when set, names the gateway stage that
// must refuse the step with GatewayStatus before it reaches any fixture.
type Scenario struct {
	Name          string          `json:"name"`
	Subject       string          `json:"subject,omitempty"`
	Operation     string          `json:"operation"`
	Method        string          `json:"method"`
	Path          string          `json:"path"`
	Body          json.RawMessage `json:"body,omitempty"`
	GatewayStage  string          `json:"gateway_stage,omitempty"`
	GatewayStatus int             `json:"gateway_status,omitempty"`
	Status        int             `json:"status"`
	ChangesState  bool            `json:"changes_state"`
	Effect        string          `json:"effect,omitempty"`
	Response      json.RawMessage `json:"response,omitempty"`
}

// LoadCatalogue decodes the embedded catalogue, rejecting unknown fields.
func LoadCatalogue() (Catalogue, error) {
	var c Catalogue
	decoder := json.NewDecoder(bytes.NewReader(catalogueJSON))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&c)
	return c, err
}
