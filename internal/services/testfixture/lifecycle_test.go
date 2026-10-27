package testfixture

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestRunClosesActiveRequestAfterShutdownDeadline(t *testing.T) {
	addresses := make([]string, 2)
	listeners := make([]net.Listener, 2)
	for i := range listeners {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners[i] = listener
		addresses[i] = listener.Addr().String()
		t.Cleanup(func() { listener.Close() })
	}
	for _, listener := range listeners {
		listener.Close()
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	service, err := New("inventory")
	if err != nil {
		t.Fatal(err)
	}
	service.operations = []operation{{method: "GET", pattern: "/waiting", run: func(_ string, r *http.Request, _ []byte) result {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
		return result{status: 200}
	}}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	finished := make(chan struct{})
	go func() {
		stopped <- Run(ctx, service, addresses[0], addresses[1], make([]byte, minSecretBytes))
		close(finished)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-finished:
		case <-time.After(7 * time.Second):
			t.Error("fixture did not stop during cleanup")
		}
	})

	var connection net.Conn
	deadline := time.Now().Add(5 * time.Second)
	for connection == nil {
		connection, err = net.DialTimeout("tcp", addresses[0], 100*time.Millisecond)
		if err != nil {
			if time.Now().After(deadline) {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	defer connection.Close()
	if _, err := io.WriteString(connection, "GET /waiting HTTP/1.1\r\nHost: fixture\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not enter the handler")
	}
	cancel()
	// Exercise Run's real five-second grace period, then require forced cleanup
	// and an honest error rather than a still-live connection reported as stopped.
	select {
	case err := <-stopped:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("shutdown error = %v, want deadline exceeded", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("Run did not return after its shutdown deadline")
	}
	connection.SetReadDeadline(time.Now().Add(time.Second))
	_, err = connection.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("request produced a response instead of closing at the shutdown deadline")
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatal("Run returned with an active request connection still open")
	}
}
