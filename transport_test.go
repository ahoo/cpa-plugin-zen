package plugin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type recordingHostHTTPClient struct {
	requests []pluginapi.HTTPRequest
	response pluginapi.HTTPResponse
}

func (h *recordingHostHTTPClient) Do(_ context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	h.requests = append(h.requests, req)
	return h.response, nil
}

func (*recordingHostHTTPClient) DoStream(context.Context, pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	return pluginapi.HTTPStreamResponse{}, errors.New("unexpected streaming request")
}

func TestRawHostHTTPMethodPreserved(t *testing.T) {
	host := &recordingHostHTTPClient{response: pluginapi.HTTPResponse{StatusCode: http.StatusNoContent}}
	e := &Executor{cfg: testConfig(), keypool: newPool()}
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPatch, ""} {
		t.Run(method, func(t *testing.T) {
			resp, err := e.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{
				Method: method, URL: "https://example.invalid/resource", Headers: http.Header{"X-Test": {"value"}}, Body: []byte("payload"), HTTPClient: host,
			})
			if err != nil || resp.StatusCode != http.StatusNoContent {
				t.Fatalf("HttpRequest: response=%+v error=%v", resp, err)
			}
			got := host.requests[len(host.requests)-1]
			if got.Method != method || got.Headers.Get("X-Test") != "value" || got.Headers.Get("Authorization") != "Bearer sk-test" || !bytes.Equal(got.Body, []byte("payload")) {
				t.Fatalf("host request = %+v, want method %q and unchanged headers/body", got, method)
			}
		})
	}
}

func TestPaidModelCallsArePOST(t *testing.T) {
	host := &recordingHostHTTPClient{response: pluginapi.HTTPResponse{
		StatusCode: http.StatusOK,
		Body:       []byte(`{"model":"jev-1.13","answers":{"u":{"type":"noul","noul":0.5}}}`),
	}}
	e := &Executor{cfg: testConfig(), keypool: newPool()}
	payload := []byte(`{"model":"jev","messages":[{"role":"user","content":"{\"state\":\"Hi.\",\"questions\":{\"u\":{\"type\":\"noul\",\"instructions\":\"Urgent?\"}}}"}]}`)
	for _, streaming := range []bool{false, true} {
		req := pluginapi.ExecutorRequest{Model: "jev", Payload: payload, HTTPClient: host}
		if streaming {
			resp, err := e.ExecuteStream(context.Background(), req)
			if err != nil {
				t.Fatalf("ExecuteStream: %v", err)
			}
			for range resp.Chunks {
			}
		} else if _, err := e.Execute(context.Background(), req); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		got := host.requests[len(host.requests)-1]
		if got.Method != http.MethodPost || got.URL != e.endpoint() || got.Headers.Get("Authorization") != "Bearer sk-test" || len(got.Body) == 0 {
			t.Fatalf("paid model request = %+v, want POST with auth and body", got)
		}
	}
}

func TestStdDoerMethodAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		if r.Method != http.MethodPut || r.Header.Get("X-Test") != "value" || !bytes.Equal(body, []byte{0, 255, 1}) {
			t.Errorf("request method=%q header=%q body=%v", r.Method, r.Header.Get("X-Test"), body)
		}
		w.Header().Set("X-Reply", "ok")
		_, _ = w.Write([]byte("response"))
	}))
	defer server.Close()
	status, headers, body, err := (stdDoer{client: server.Client()}).do(context.Background(), http.MethodPut, server.URL, http.Header{"X-Test": {"value"}}, []byte{0, 255, 1})
	if err != nil || status != http.StatusOK || headers.Get("X-Reply") != "ok" || string(body) != "response" {
		t.Fatalf("stdDoer response: status=%d headers=%v body=%q error=%v", status, headers, body, err)
	}
}

func TestStdDoerEmptyMethodDefaultsToGET(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("empty method resolved to %q, want GET", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	status, _, _, err := (stdDoer{client: server.Client()}).do(context.Background(), "", server.URL, nil, nil)
	if err != nil || status != http.StatusNoContent {
		t.Fatalf("empty-method request: status=%d error=%v", status, err)
	}
}

func TestRawProxiedHTTPMethod(t *testing.T) {
	proxyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.String() != "http://example.invalid/item" {
			t.Errorf("proxy received method %q, URL %q", r.Method, r.URL)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer proxyServer.Close()
	cfg := testConfig()
	cfg.Paid.APIKeys[0].ProxyURL = proxyServer.URL
	e := &Executor{cfg: cfg, keypool: newPool()}
	resp, err := e.HttpRequest(context.Background(), pluginapi.ExecutorHTTPRequest{Method: http.MethodDelete, URL: "http://example.invalid/item"})
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("proxied HttpRequest: response=%+v error=%v", resp, err)
	}
}

func TestStdDoerPropagatesUnexpectedEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100")
		_, _ = w.Write([]byte("partial"))
	}))
	defer server.Close()
	_, _, body, err := (stdDoer{client: server.Client()}).do(context.Background(), http.MethodGet, server.URL, nil, nil)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short upstream body: body=%q error=%v, want unexpected EOF", body, err)
	}
	if len(body) != 0 {
		t.Fatalf("partial response must not be returned as success: %q", body)
	}
}

func TestStdDoerDoesNotAcceptCanceledBody(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, _, body, err := (stdDoer{client: server.Client()}).do(ctx, http.MethodGet, server.URL, nil, nil)
		if len(body) != 0 {
			result <- errors.New("canceled response returned partial body")
			return
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("test server did not start response")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled response returned %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("response read did not stop after cancellation")
	}
}

func TestSOCKSDialCancelsDuringHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	transport, err := proxyTransport("socks5://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		conn, err := transport.(*http.Transport).DialContext(ctx, "tcp", "example.invalid:80")
		if conn != nil {
			_ = conn.Close()
		}
		result <- err
	}()
	var conn net.Conn
	select {
	case conn = <-accepted:
		defer conn.Close()
	case <-time.After(time.Second):
		t.Fatal("SOCKS dial did not reach local listener")
	}
	// Keep the proxy's handshake response pending while cancellation arrives.
	greeting := make([]byte, 3)
	if _, err := io.ReadFull(conn, greeting); err != nil {
		t.Fatalf("SOCKS greeting: %v", err)
	}
	cancel()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("canceled SOCKS dial succeeded")
		}
	case <-time.After(time.Second):
		_ = conn.Close() // release the old non-context-aware implementation
		<-result
		t.Fatal("SOCKS dial ignored context cancellation")
	}
}

func TestProxyTransportErrorsDoNotExposeCredentials(t *testing.T) {
	for _, raw := range []string{"socks5://user:secret@bad-host:invalid", "http://user:secret@%broken", "user:secret@proxy.invalid"} {
		_, err := proxyTransport(raw)
		if err == nil {
			continue // syntactically valid proxy URLs can fail only when dialed
		}
		if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "user:") {
			t.Fatalf("proxy validation error exposed credentials: %v", err)
		}
	}
}
