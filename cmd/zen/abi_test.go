package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestABIBufferBounds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ptr      bool
		n, limit uint64
		bad      bool
	}{
		{"empty", false, 0, math.MaxInt32, false},
		{"empty-nonnull", true, 0, math.MaxInt32, false},
		{"nil-payload", false, 1, math.MaxInt32, true},
		{"max-int", true, math.MaxInt32, math.MaxInt32, false},
		{"overflow", true, math.MaxInt32 + 1, math.MaxUint64, true},
		{"max-size", true, math.MaxUint64, math.MaxInt32, true},
		{"interceptor-limit", true, maxInterceptPayloadBytes, maxInterceptPayloadBytes, false},
		{"interceptor-overflow", true, maxInterceptPayloadBytes + 1, maxInterceptPayloadBytes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateABIBuffer(tc.ptr, tc.n, tc.limit); (err != nil) != tc.bad {
				t.Fatalf("validate: %v, want error=%v", err, tc.bad)
			}
		})
	}
}

func TestStreamPumpFailureCancelsProducer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	chunks := make(chan pluginapi.ExecutorStreamChunk)
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		defer close(chunks)
		for {
			select {
			case <-ctx.Done():
				return
			case chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte(`{"choices":[]}`)}:
			}
		}
	}()
	// There is no host callback in this process. The first emit must fail
	// and stop the producer, including sends past the channel capacity.
	_, err := marshalABIStreamResponse(ctx, cancel, "test-stream", pluginapi.ExecutorStreamResponse{Chunks: chunks})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("failed emit leaked producer")
	}
}

func TestHTTPStreamSendCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan pluginapi.HTTPStreamChunk)
	exited := make(chan struct{})
	go func() { defer close(exited); readHostHTTPStream(ctx, "test-http", out) }()
	cancel()
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("host stream send ignored cancellation")
	}
}

type testStatusError int

func (e testStatusError) Error() string   { return "test status" }
func (e testStatusError) StatusCode() int { return int(e) }

func TestWrappedStatusPreserved(t *testing.T) {
	raw := abiErrorEnvelopeFromError("plugin_error", fmt.Errorf("wrapped: %w", testStatusError(429)))
	if !bytes.Contains(raw, []byte(`"http_status":429`)) {
		t.Fatalf("wrapped status lost: %s", raw)
	}
	const marker = "earliest_local_retry_at=unknown"
	raw = abiErrorEnvelopeFromError("plugin_error", fmt.Errorf("zen free executor: %s: %w", marker, testStatusError(503)))
	var envelope struct {
		OK    bool `json:"ok"`
		Error struct {
			Code       string `json:"code"`
			Message    string `json:"message"`
			HTTPStatus int    `json:"http_status"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error.Code != "plugin_error" || envelope.Error.HTTPStatus != 503 || !bytes.Contains([]byte(envelope.Error.Message), []byte(marker)) {
		t.Fatalf("local 503 status/message lost: %s", raw)
	}
}

func TestShutdownDrainsWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	abiState.Lock()
	abiState.ctx, abiState.cancel = ctx, cancel
	abiState.closing = false
	abiState.Unlock()
	defer func() {
		abiState.Lock()
		abiState.closing = false
		abiState.Unlock()
	}()
	canceled, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	if !startABIWorker(func() { <-ctx.Done(); close(canceled); <-release }) {
		t.Fatal("worker rejected before shutdown")
	}
	go func() { ZenPluginShutdown(); close(done) }()
	select {
	case <-canceled:
	case <-time.After(time.Second):
		close(release)
		t.Fatal("shutdown did not cancel workers")
	}
	if startABIWorker(func() {}) {
		t.Error("shutdown admitted a new worker")
	}
	select {
	case <-done:
		t.Error("shutdown returned before worker cleanup")
	default:
	}
	close(release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not drain")
	}
}

func TestBufferedStreamPreservesError(t *testing.T) {
	want := errors.New("upstream interrupted")
	chunks := make(chan pluginapi.ExecutorStreamChunk, 2)
	chunks <- pluginapi.ExecutorStreamChunk{Payload: []byte(`{"choices":[]}`)}
	chunks <- pluginapi.ExecutorStreamChunk{Err: want}
	close(chunks)
	_, err := marshalABIStreamResponse(context.Background(), func() {}, "", pluginapi.ExecutorStreamResponse{Chunks: chunks})
	if !errors.Is(err, want) {
		t.Fatalf("buffered stream error = %v, want %v", err, want)
	}
}

func TestBufferedStreamCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := marshalABIStreamResponse(ctx, cancel, "", pluginapi.ExecutorStreamResponse{Chunks: make(chan pluginapi.ExecutorStreamChunk)})
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("buffered stream ignored cancellation")
	}
}
