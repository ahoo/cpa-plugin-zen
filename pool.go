package plugin

import (
	"context"
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
	"golang.org/x/net/proxy"
)

// pool is a multi-key pool: weighted random selection with per-key proxy
// transports and failover retry across members.
//
// Credential sources, in order:
//  1. cfg.APIKeys (weighted pool)
//  2. cfg.APIKey (legacy single key, weight 1, no proxy)
//  3. req.AuthAttributes/AuthMetadata api_key (only when the host passes a
//     real auth; on the ModelRouter path it passes nil)
type pool struct {
	mu      sync.Mutex
	rnd     *rand.Rand
	clients map[int]*http.Client // keyed by member index; lazily built
}

func newPool() *pool {
	return &pool{rnd: rand.New(rand.NewSource(time.Now().UnixNano())), clients: map[int]*http.Client{}}
}

// poolReqFromHTTP adapts an ExecutorHTTPRequest to the member resolver.
func poolReqFromHTTP(req pluginapi.ExecutorHTTPRequest) pluginapi.ExecutorRequest {
	return pluginapi.ExecutorRequest{AuthAttributes: req.Attributes, AuthMetadata: req.Metadata}
}

// members resolves the effective pool from config + request fallback.
func (c *pluginConfig) members(req pluginapi.ExecutorRequest) []APIKeyEntry {
	if c != nil && len(c.Paid.APIKeys) > 0 {
		out := make([]APIKeyEntry, 0, len(c.Paid.APIKeys))
		for _, en := range c.Paid.APIKeys {
			if strings.TrimSpace(en.Key) == "" {
				continue
			}
			out = append(out, en)
		}
		if len(out) > 0 {
			return out
		}
	}
	if c != nil && strings.TrimSpace(c.Paid.APIKey) != "" {
		return []APIKeyEntry{{Key: strings.TrimSpace(c.Paid.APIKey), Weight: 1}}
	}
	if k := strings.TrimSpace(req.AuthAttributes["api_key"]); k != "" {
		return []APIKeyEntry{{Key: k, Weight: 1}}
	}
	if req.AuthMetadata != nil {
		if k, ok := req.AuthMetadata["api_key"].(string); ok && strings.TrimSpace(k) != "" {
			return []APIKeyEntry{{Key: strings.TrimSpace(k), Weight: 1}}
		}
	}
	return nil
}

// order returns member indices in weighted-random order (no repeats).
func (p *pool) order(members []APIKeyEntry) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	remaining := make([]int, len(members))
	for i := range remaining {
		remaining[i] = i
	}
	out := make([]int, 0, len(members))
	for len(remaining) > 0 {
		weights := 0
		for _, i := range remaining {
			weights += members[i].normWeight()
		}
		r := p.rnd.Intn(weights)
		pick := 0
		for i, idx := range remaining {
			r -= members[idx].normWeight()
			if r < 0 {
				pick = i
				break
			}
		}
		out = append(out, remaining[pick])
		remaining = append(remaining[:pick], remaining[pick+1:]...)
	}
	return out
}

// clientFor returns the HTTP client for a member: the host client when the
// member has no proxy_url (keeps host proxy policy + request-log), otherwise
// a self-built client with the member's proxy transport.
func (p *pool) clientFor(idx int, member APIKeyEntry, hostClient pluginapi.HostHTTPClient) (doer, error) {
	if strings.TrimSpace(member.ProxyURL) == "" {
		if hostClient == nil {
			return nil, fmt.Errorf("zen executor: host HTTP client is required")
		}
		return hostDoer{client: hostClient}, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[idx]; ok {
		return stdDoer{client: c}, nil
	}
	transport, err := proxyTransport(strings.TrimSpace(member.ProxyURL))
	if err != nil {
		return nil, err
	}
	c := &http.Client{Transport: transport, Timeout: 0}
	p.clients[idx] = c
	return stdDoer{client: c}, nil
}

// doer abstracts host vs self-built HTTP execution. SystemOne upstream is a
// single-shot JSON endpoint, so only non-streaming calls are needed.
type doer interface {
	do(ctx context.Context, url string, headers http.Header, body []byte) (status int, respHeaders http.Header, respBody []byte, err error)
}

type hostDoer struct{ client pluginapi.HostHTTPClient }

func (d hostDoer) do(ctx context.Context, url string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	resp, err := d.client.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodPost, URL: url, Headers: headers, Body: body})
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Headers, resp.Body, nil
}

type stdDoer struct{ client *http.Client }

func (d stdDoer) do(ctx context.Context, url string, headers http.Header, body []byte) (int, http.Header, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header = headers.Clone()
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	respBody := make([]byte, 0, 4096)
	buf := make([]byte, 32*1024)
	for {
		n, errRead := resp.Body.Read(buf)
		if n > 0 {
			respBody = append(respBody, buf[:n]...)
		}
		if errRead != nil {
			break
		}
	}
	return resp.StatusCode, resp.Header, respBody, nil
}

// proxyTransport builds an http.RoundTripper honoring http/https/socks5
// proxy URLs (with optional user:pass credentials).
func proxyTransport(proxyURL string) (http.RoundTripper, error) {
	u, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("zen executor: invalid proxy_url %q: %w", proxyURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return &http.Transport{
			Proxy:           http.ProxyURL(u),
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
			DialContext:     (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		}, nil
	case "socks5", "socks5h":
		dialer, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("zen executor: invalid socks proxy %q: %w", proxyURL, err)
		}
		return &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		}, nil
	case "":
		return nil, fmt.Errorf("zen executor: proxy_url %q missing scheme", proxyURL)
	default:
		return nil, fmt.Errorf("zen executor: unsupported proxy scheme %q", u.Scheme)
	}
}

// retryable reports whether an upstream failure is worth failing over to
// the next pool member: transport errors, 429, 401 and 5xx. Other 4xx
// (400/403/404/422) fail fast: the request itself is bad, retrying another
// key won't help.
func retryable(status int, err error) bool {
	if err != nil {
		return true
	}
	if status == 401 || status == 429 || (status >= 500 && status <= 599) {
		return true
	}
	return false
}
