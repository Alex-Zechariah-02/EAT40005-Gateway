package services

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

type serviceClient struct {
	base   string
	secret string
	http   *http.Client
}

func newServiceClient(base string, secret []byte) (*serviceClient, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	address, err := netip.ParseAddrPort(parsed.Host)
	// Each call appends its path to base, so base must end at the port: even an
	// empty query or fragment delimiter would turn that path into its query or
	// fragment and send the call to "/".
	if err != nil || parsed.Scheme != "http" || !address.Addr().IsLoopback() || address.Port() == 0 ||
		parsed.User != nil || parsed.Path != "" || strings.ContainsAny(base, "?#") {
		return nil, errors.New("internal service destination must be an HTTP literal loopback origin")
	}
	return &serviceClient{base: base, secret: string(secret), http: &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil, ResponseHeaderTimeout: 2 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *serviceClient) call(ctx context.Context, method, path string, body []byte, key string) (int, []byte, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("X-Service-Key", c.secret)
	if len(body) > 0 {
		request.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil {
		return 0, nil, err
	}
	if len(data) > 1<<20 {
		return 0, nil, errors.New("internal response exceeds its bound")
	}
	return response.StatusCode, data, nil
}
func (c *serviceClient) close() { c.http.CloseIdleConnections() }
