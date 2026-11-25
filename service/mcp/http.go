package mcp

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// Each client is pinned to one operator-configured origin. Redirects and remote
// SSE message endpoints cannot move service credentials to another origin.
func NewHTTPClient(endpoint string, timeout time.Duration) (*http.Client, error) {
	u, err := ValidateEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	origin := strings.ToLower(u.Scheme + "://" + u.Host)
	// Administrator-configured upstreams may use HTTP and private networks.
	policy := &common.SSRFProtection{AllowPrivateIp: true}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout,
		IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 4, ForceAttemptHTTP2: true}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil || !strings.EqualFold(host, u.Hostname()) {
			return nil, Failure(502, "UPSTREAM_ADDRESS_BLOCKED", "MCP upstream host is not allowed")
		}
		portNumber, err := strconv.Atoi(port)
		if err != nil || policy.ValidateNetworkTarget(host, portNumber) != nil {
			return nil, Failure(502, "UPSTREAM_ADDRESS_BLOCKED", "MCP upstream address is not allowed")
		}
		addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(addresses) == 0 {
			return nil, Failure(502, "UPSTREAM_DNS_FAILED", "MCP upstream DNS lookup failed")
		}
		for _, address := range addresses {
			if err = policy.ValidateResolvedIP(host, address.IP); err != nil {
				return nil, Failure(502, "UPSTREAM_ADDRESS_BLOCKED", "MCP upstream address is not allowed")
			}
		}
		var lastErr error
		for _, address := range addresses {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(address.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
		}
		return nil, lastErr
	}
	return &http.Client{Transport: &originTransport{base: transport, origin: origin}, Timeout: timeout,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 3 || !strings.EqualFold(request.URL.Scheme+"://"+request.URL.Host, origin) {
				return http.ErrUseLastResponse
			}
			return nil
		}}, nil
}

func ValidateEndpoint(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || len(value) > 4096 || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, Failure(422, "INVALID_ENDPOINT", "A configured HTTP(S) endpoint without credentials, query or fragment is required")
	}
	return u, nil
}

type originTransport struct {
	base   http.RoundTripper
	origin string
}

func (t *originTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.User != nil || !strings.EqualFold(request.URL.Scheme+"://"+request.URL.Host, t.origin) {
		return nil, fmt.Errorf("MCP upstream origin is not allowed")
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = &boundedResponseBody{ReadCloser: response.Body, remaining: MaxResultBytes}
	return response, nil
}
func (t *originTransport) CloseIdleConnections() {
	if transport, ok := t.base.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}
}

type boundedResponseBody struct {
	io.ReadCloser
	remaining int64
}

func (b *boundedResponseBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		var extra [1]byte
		n, err := b.ReadCloser.Read(extra[:])
		if n > 0 {
			return 0, fmt.Errorf("MCP upstream response exceeds the limit")
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.ReadCloser.Read(p)
	b.remaining -= int64(n)
	return n, err
}

// requestHTTPPool owns connections for one tool invocation, never credentials or
// authorization decisions. Origin and timeout policies remain isolated.
type requestHTTPPool struct {
	mu      sync.Mutex
	clients map[string]*http.Client
	closed  bool
}
type requestHTTPPoolKey struct{}

// borrowedTransport leaves connection cleanup to the invocation owner.
type borrowedTransport struct{ http.RoundTripper }

func (pool *requestHTTPPool) Close() {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	pool.closed = true
	for _, client := range pool.clients {
		client.CloseIdleConnections()
	}
}

func requestHTTPClient(ctx context.Context, endpoint string, timeout time.Duration) (*http.Client, error) {
	pool, ok := ctx.Value(requestHTTPPoolKey{}).(*requestHTTPPool)
	if !ok {
		return NewHTTPClient(endpoint, timeout)
	}
	u, err := ValidateEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	key := strings.ToLower(u.Scheme+"://"+u.Host) + "\x00" + timeout.String()
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.closed {
		return NewHTTPClient(endpoint, timeout)
	}
	client := pool.clients[key]
	if client == nil {
		client, err = NewHTTPClient(endpoint, timeout)
		if err != nil {
			return nil, err
		}
		if pool.clients == nil {
			pool.clients = make(map[string]*http.Client)
		}
		pool.clients[key] = client
	}
	borrowed := *client
	borrowed.Transport = borrowedTransport{client.Transport}
	return &borrowed, nil
}
