package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOperatorBoundaryRefusals(t *testing.T) {
	for _, args := range [][]string{
		{"-scenario", "unknown"},
		{"-scenario", "flood-search", "-from", "8.8.8.8"},
		{"-scenario", "flood-search", "-from", "::1"},
		{"-scenario", "flood-search", "-count", "0"},
		{"-scenario", "flood-search", "-count", "10001"},
	} {
		if attack(append(args, "-dir", t.TempDir())) == nil {
			t.Errorf("attack accepted %v", args)
		}
	}
	for _, args := range [][]string{{"retire", "-dir", t.TempDir()}, {"unknown"}} {
		if keys(args) == nil {
			t.Errorf("keys accepted %v", args)
		}
	}
	for _, args := range [][]string{{"export", "-dir", t.TempDir()}, {"stage", "-dir", t.TempDir(), "-file", filepath.Join(t.TempDir(), "missing.json")}, {"unknown"}} {
		if policy(args) == nil {
			t.Errorf("policy accepted %v", args)
		}
	}
	path := filepath.Join(t.TempDir(), "owned.txt")
	if err := writeNewFile(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writeNewFile(path, []byte("second")); err == nil {
		t.Fatal("export overwrote an existing file")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "first" {
		t.Fatal("exclusive output did not preserve existing data")
	}
	if err := replaceOperatorFile(filepath.Dir(path), []byte("invalid")); err == nil {
		t.Fatal("operator replaced a directory")
	}
}

func operatorTestState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string][]byte{
		configFile:  []byte(`{"gateway":{"jwt":{"keys":[{"kid":"active","public_key_file":"active.pem"}]}}}`),
		secretFile:  []byte("01234567890123456789012345678901"),
		signingFile: []byte(`{"kid":"active","private_key_file":"active-key.pem"}`),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := identifyDemo(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestKeyOperatorRefusalsAndUnconfirmedReload(t *testing.T) {
	dir := operatorTestState(t)
	before, err := os.ReadFile(filepath.Join(dir, configFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, kid := range []string{"active", "unknown"} {
		if _, err := changeKeys(dir, newAdminClient(), "retire", kid); err == nil {
			t.Fatalf("retired %s", kid)
		}
	}
	after, _ := os.ReadFile(filepath.Join(dir, configFile))
	if string(before) != string(after) {
		t.Fatal("refused retirement changed operator settings")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(422) }))
	defer server.Close()
	old := adminListen
	adminListen = strings.TrimPrefix(server.URL, "http://")
	defer func() { adminListen = old }()
	if _, err := changeKeys(dir, newAdminClient(), "rotate", ""); err == nil {
		t.Fatal("unconfirmed reload reported success")
	}
	signing, _ := os.ReadFile(filepath.Join(dir, signingFile))
	if !strings.Contains(string(signing), `"kid":"active"`) {
		t.Fatal("Accounts switched before gateway reload was confirmed")
	}
	if err := checkDemo(dir); err != nil {
		t.Fatalf("authorized settings update lost deployment identity: %v", err)
	}
}

func TestAdminUnknownWriteIsNotRetried(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.WriteHeader(200)
			return
		}
		if writes.Add(1) == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	old := adminListen
	adminListen = strings.TrimPrefix(server.URL, "http://")
	defer func() { adminListen = old }()
	client := newAdminClient()
	defer client.client.CloseIdleConnections()
	if _, _, err := client.send("GET", "/warm", nil, nil, 200); err != nil {
		t.Fatal(err)
	}
	if _, _, err := client.send("POST", "/write", nil, mutationHeaders(""), 200); err == nil {
		t.Fatal("lost answer was reported as success")
	}
	if writes.Load() != 1 {
		t.Fatalf("unknown write automatically retried %d times", writes.Load())
	}
}

func TestAdminDoesNotFollowWriteRedirect(t *testing.T) {
	var writes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes.Add(1)
		if r.URL.Path == "/write" {
			w.Header().Set("Location", "/repeated")
			w.WriteHeader(307)
			return
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	old := adminListen
	adminListen = strings.TrimPrefix(server.URL, "http://")
	defer func() { adminListen = old }()
	client := newAdminClient()
	defer client.client.CloseIdleConnections()
	if _, _, err := client.send("POST", "/write", nil, mutationHeaders(""), 200); err == nil {
		t.Error("operator followed a write redirect")
	}
	if writes.Load() != 1 {
		t.Errorf("operator sent %d writes for one intent", writes.Load())
	}
}
