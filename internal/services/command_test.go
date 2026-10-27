package services

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestServiceCommandValidation(t *testing.T) {
	for _, args := range [][]string{{}, {"-service", "unknown"}, {"-service", "inventory"}, {"-service", "inventory", "-secret-file", "missing.secret"}} {
		if err := Command(context.Background(), "", args, io.Discard); err == nil {
			t.Fatalf("invalid command accepted: %v", args)
		}
	}
	t.Setenv("EAT_PORT_OFFSET", "65535")
	if err := Command(context.Background(), "", []string{"-service", "inventory"}, io.Discard); err == nil {
		t.Fatal("overflowing offset accepted")
	}
}

func freeServiceAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

func TestServiceCommandLifecycle(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "service.secret")
	if err := os.WriteFile(secretFile, testSecret, 0600); err != nil {
		t.Fatal(err)
	}
	_, _, signingFile := accountService(t)
	addresses := map[string]string{}
	client := &http.Client{Timeout: time.Second}
	for _, name := range []string{"inventory", "orders", "reports", "support", "accounts"} {
		api, control := freeServiceAddress(t), freeServiceAddress(t)
		addresses[name] = api
		args := []string{"-listen", api, "-control", control, "-secret-file", secretFile}
		fixed := ""
		if name == "accounts" {
			fixed = "accounts"
			args = append(args, "-signing-settings", signingFile)
		} else {
			args = append(args, "-service", name)
		}
		if name == "orders" {
			args = append(args, "-inventory", "http://"+addresses["inventory"])
		}
		if name == "reports" {
			args = append(args, "-orders", "http://"+addresses["orders"])
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- Command(ctx, fixed, args, io.Discard) }()
		t.Cleanup(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("%s shutdown: %v", name, err)
				}
			case <-time.After(7 * time.Second):
				t.Errorf("%s did not stop", name)
			}
		})
		deadline := time.Now().Add(5 * time.Second)
		for {
			request, _ := http.NewRequest("GET", "http://"+control+"/state", nil)
			request.Header.Set("Authorization", "Bearer "+string(testSecret))
			response, err := client.Do(request)
			if err == nil {
				response.Body.Close()
				if response.StatusCode != 200 || response.Header.Get("X-Service") != name {
					t.Fatalf("%s readiness: %d", name, response.StatusCode)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s startup: %v", name, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		paths := map[string]string{"inventory": "/inventory/categories", "orders": "/orders/wallet", "reports": "/reports/summary", "support": "/support/mine", "accounts": "/accounts/me"}
		request, _ := http.NewRequest("GET", "http://"+api+paths[name], nil)
		request.Header.Set("X-Gateway-Subject", "cust-001")
		request.Header.Set("X-Gateway-Roles", "CUSTOMER")
		if name == "reports" {
			request.Header.Set("X-Gateway-Subject", "ops-01")
			request.Header.Set("X-Gateway-Roles", "OPERATIONS")
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != 200 || response.Header.Get("X-Service") != name {
			t.Fatalf("%s endpoint: %d", name, response.StatusCode)
		}
	}
}
