// Integration harness data-plane requests and JSON helpers.

package gateway

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func (e *env) get(path string, header ...string) (int, string) {
	return e.request(http.MethodGet, path, "", "", header...)
}

func (e *env) request(method, path, contentType, body string, header ...string) (int, string) {
	e.t.Helper()
	resp, data := e.exchange(method, path, contentType, body, header...)
	return resp.StatusCode, data
}

// exchange sends one request to the data plane and returns the response,
// whose body has been read into the returned string.
func (e *env) exchange(method, path, contentType, body string, header ...string) (*http.Response, string) {
	e.t.Helper()
	req, err := http.NewRequest(method, "http://"+e.dataAddr+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if body == "" {
		req.Body = http.NoBody
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

// raw sends bytes exactly as given, bypassing client-side normalisation.
func (e *env) raw(request string) (int, string) {
	e.t.Helper()
	resp, data := e.rawResponse(request)
	return resp.StatusCode, data
}

// rawResponse is raw returning the whole response, whose body has been read
// into the returned string.
func (e *env) rawResponse(request string) (*http.Response, string) {
	e.t.Helper()
	conn, err := net.DialTimeout("tcp", e.dataAddr, 5*time.Second)
	if err != nil {
		e.t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		e.t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		e.t.Fatalf("reading response to %q: %v", request, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

// anyObject is a body schema that accepts any JSON object.
const anyObject = `{"type":"object","additionalProperties":true}`

// jsonBody declares a JSON body with the given size limit and schema.
func jsonBody(maxBytes int64, schema string) *BodyRule {
	return &BodyRule{MaxBytes: maxBytes, Schema: json.RawMessage(schema)}
}

func mustJSON(v any) json.RawMessage {
	data, _ := json.Marshal(v)
	return data
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
