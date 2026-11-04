// Forwarded responses.

package gateway

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Responses pass through as the upstream sent them, without the upstream's
// hop-by-hop fields: status, headers and exact body bytes, informational
// responses, redirects (never followed), trailers, streamed parts as they
// arrive, and encoded bodies (never decoded). An upstream that switches
// protocols is not followed into a tunnel.
func TestNativeForwardingResponses(t *testing.T) {
	e := newEnv(t)
	e.serveNative(e.policy(e.a, false), forwardTimeouts)
	var respond func(w http.ResponseWriter, r *http.Request)
	var mu sync.Mutex
	e.a.handle = func(w http.ResponseWriter, r *http.Request) bool {
		mu.Lock()
		f := respond
		mu.Unlock()
		f(w, r)
		return true
	}
	set := func(f func(w http.ResponseWriter, r *http.Request)) {
		mu.Lock()
		respond = f
		mu.Unlock()
	}

	t.Run("status, headers and bytes", func(t *testing.T) {
		binary := make([]byte, 512)
		for i := range binary {
			binary[i] = byte(i)
		}
		set(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-Upstream", "kept")
			w.Header().Set("Content-Length", fmt.Sprint(len(binary)))
			w.WriteHeader(http.StatusAccepted)
			w.Write(binary)
		})
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
		if resp.StatusCode != http.StatusAccepted || body != string(binary) || resp.ContentLength != int64(len(binary)) || resp.Header.Get("X-Upstream") != "kept" {
			t.Fatalf("response %d, %d bytes (intact %v), headers %v", resp.StatusCode, len(body), body == string(binary), resp.Header)
		}
	})

	t.Run("informational then final", func(t *testing.T) {
		set(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Link", "</style.css>; rel=preload")
			w.WriteHeader(http.StatusEarlyHints)
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "final")
		})
		resp, conn := e.openResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
		if resp.StatusCode != http.StatusEarlyHints || resp.Header.Get("Link") == "" {
			t.Fatalf("first response %d %v", resp.StatusCode, resp.Header)
		}
		final, err := http.ReadResponse(bufio.NewReader(io.MultiReader(resp.Body, conn)), nil)
		if err != nil || final.StatusCode != http.StatusOK {
			t.Fatalf("final response %v: %v", final, err)
		}
	})

	t.Run("redirect not followed", func(t *testing.T) {
		set(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://"+e.c.addr+"/a/x", http.StatusFound)
		})
		resp, _ := e.rawResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "http://"+e.c.addr+"/a/x" || e.c.count() != 0 {
			t.Fatalf("redirect: %d, Location %q, receipts at C %d", resp.StatusCode, resp.Header.Get("Location"), e.c.count())
		}
	})

	t.Run("trailers", func(t *testing.T) {
		set(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Trailer", "X-Checksum")
			io.WriteString(w, "body")
			w.Header().Set("X-Checksum", "c0ffee")
		})
		resp, body := e.exchange(http.MethodGet, "/a/x", "", "")
		if resp.StatusCode != http.StatusOK || body != "body" || resp.Trailer.Get("X-Checksum") != "c0ffee" {
			t.Fatalf("response %d %q, trailers %v", resp.StatusCode, body, resp.Trailer)
		}
	})

	t.Run("streamed as it arrives", func(t *testing.T) {
		next := make(chan struct{})
		set(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "first\n")
			w.(http.Flusher).Flush()
			select {
			case <-next:
			case <-time.After(10 * time.Second):
			}
			io.WriteString(w, "second\n")
		})
		resp, _ := e.openResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
		reader := bufio.NewReader(resp.Body)
		if line, err := reader.ReadString('\n'); err != nil || line != "first\n" {
			t.Fatalf("first part: %q %v", line, err)
		}
		close(next)
		if rest, err := io.ReadAll(reader); err != nil || string(rest) != "second\n" {
			t.Fatalf("second part: %q %v", rest, err)
		}
	})

	t.Run("encoded body not decoded", func(t *testing.T) {
		var compressed bytes.Buffer
		zw := gzip.NewWriter(&compressed)
		io.WriteString(zw, strings.Repeat("payload ", 64))
		zw.Close()
		set(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Accept-Encoding") != "gzip" {
				w.WriteHeader(http.StatusNotAcceptable)
				return
			}
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", fmt.Sprint(compressed.Len()))
			w.Write(compressed.Bytes())
		})
		resp, body := e.rawResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\nAccept-Encoding: gzip\r\nConnection: close\r\n\r\n")
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "gzip" || body != compressed.String() ||
			resp.ContentLength != int64(compressed.Len()) {
			t.Fatalf("response %d, Content-Encoding %q, %d bytes (identical %v)", resp.StatusCode, resp.Header.Get("Content-Encoding"), len(body), body == compressed.String())
		}
	})

	t.Run("protocol switch refused", func(t *testing.T) {
		closed := make(chan struct{})
		set(func(w http.ResponseWriter, r *http.Request) {
			conn, buffered, _ := http.NewResponseController(w).Hijack()
			defer conn.Close()
			buffered.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
			buffered.Flush()
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := buffered.ReadByte(); errors.Is(err, io.EOF) {
				close(closed)
			}
		})
		resp, body := e.rawResponse("GET /a/x HTTP/1.1\r\nHost: gw\r\nConnection: close\r\n\r\n")
		_, id := requestID(resp, body)
		if resp.StatusCode != http.StatusBadGateway || !failedOutcome(e.outcomeOf(id), http.StatusBadGateway, "upstream_unavailable", "outcome_unknown") {
			t.Fatalf("upstream switching protocols: %d %s, outcome %v", resp.StatusCode, body, e.outcomeOf(id))
		}
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("the gateway kept the switched upstream connection open")
		}
	})
}
