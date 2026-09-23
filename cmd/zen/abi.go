// zen suite plugin ABI entrypoint (c-shared).
//
// Mirrors cmd/systemone/abi.go from cpa-plugin-systemone, plus request-interceptor dispatch, trimmed to the
// capabilities this plugin declares: model_provider, model_router,
// executor, request/response translators. SchemaVersion tracks the host
// contract (v7.2.147 => 4).
package main

/*
#include <stdint.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int ZenPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void ZenPluginFree(void*, size_t);
extern void ZenPluginShutdown(void);

static int zen_call_host(cliproxy_host_api* api, const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	return api->call(api->host_ctx, method, request, request_len, response);
}

static void zen_free_host_buffer(cliproxy_host_api* api, void* ptr, size_t len) {
	api->free_buffer(ptr, len);
}
*/
import "C"

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	plug "github.com/ahoo/cpa-plugin-zen"
)

// pluginVersion is overridden at release build time. build.sh also injects
// the library package's descriptor version with the same value.
var pluginVersion = "0.2.0"

var abiState = struct {
	sync.RWMutex
	host      *C.cliproxy_host_api
	plugin    *plug.ZenPlugin
	ctx       context.Context
	cancel    context.CancelFunc
	closing   bool
	callbacks int
	workers   int
}{}

var abiDrained = sync.NewCond(&abiState.RWMutex)

type abiLifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type abiRegistration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  abiCapabilities    `json:"capabilities"`
}

type abiCapabilities struct {
	ModelProvider         bool                         `json:"model_provider"`
	ModelRouter           bool                         `json:"model_router"`
	Executor              bool                         `json:"executor"`
	ExecutorModelScope    pluginapi.ExecutorModelScope `json:"executor_model_scope"`
	ExecutorInputFormats  []string                     `json:"executor_input_formats,omitempty"`
	ExecutorOutputFormats []string                     `json:"executor_output_formats,omitempty"`
	RequestTranslator     bool                         `json:"request_translator"`
	ResponseTranslator    bool                         `json:"response_translator"`
	RequestInterceptor    bool                         `json:"request_interceptor"`
}

type abiIdentifierResponse struct {
	Identifier string `json:"identifier"`
}

type abiExecutorRequest struct {
	pluginapi.ExecutorRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
}

type abiExecutorHTTPRequest struct {
	pluginapi.ExecutorHTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiExecutorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

type abiHostHTTPRequest struct {
	pluginapi.HTTPRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type abiHostHTTPStreamResponse struct {
	StatusCode int                         `json:"status_code"`
	Headers    http.Header                 `json:"headers,omitempty"`
	StreamID   string                      `json:"stream_id,omitempty"`
	Chunks     []pluginapi.HTTPStreamChunk `json:"chunks,omitempty"`
}

type abiHostHTTPStreamReadRequest struct {
	StreamID string `json:"stream_id"`
}

type abiHostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

type abiHostHTTPStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
}

type abiHostStreamEmitRequest struct {
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
}

type abiHostStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
}

type abiEmptyResponse struct{}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || uint32(host.abi_version) != pluginabi.ABIVersion || host.call == nil || host.free_buffer == nil {
		return 1
	}
	abiState.Lock()
	if abiState.host != nil {
		abiState.Unlock()
		return 1
	}
	abiState.host = host
	abiState.ctx, abiState.cancel = context.WithCancel(context.Background())
	abiState.closing = false
	abiState.Unlock()
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.ZenPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.ZenPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.ZenPluginShutdown)
	return 0
}

// maxInterceptPayloadBytes caps serialized interceptor envelopes (the host
// serializes the whole request, including a base64 body). Fixed and
// non-configurable; larger payloads fail closed before any copy.
const maxInterceptPayloadBytes = 64 << 20

// interceptProjection is the narrow view of the host interceptor envelope
// the session mapper needs. Body is intentionally absent: decoding it would
// base64-decode (and retain) megabytes per request for no benefit.
type interceptProjection struct {
	RequestID string              `json:"RequestID"`
	Headers   map[string][]string `json:"Headers"`
	Metadata  map[string]any      `json:"Metadata"`
}

//export ZenPluginCall
func ZenPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeABIResponse(response, abiErrorEnvelope("invalid_method", "method is required"))
		return 1
	}
	methodName := C.GoString(method)
	limit := uint64(math.MaxInt32)
	if methodName == pluginabi.MethodRequestInterceptBefore || methodName == pluginabi.MethodRequestInterceptAfter {
		limit = maxInterceptPayloadBytes
	}
	if err := validateABIBuffer(request != nil, uint64(requestLen), limit); err != nil {
		writeABIResponse(response, abiErrorEnvelope("invalid_request", err.Error()))
		return 1
	}
	var requestBytes []byte
	if requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	abiState.RLock()
	ctx := abiState.ctx
	abiState.RUnlock()
	if ctx == nil {
		ctx = context.Background()
	}
	raw, errHandle := handleABIMethod(ctx, methodName, requestBytes)
	if errHandle != nil {
		writeABIResponse(response, abiErrorEnvelopeFromError("plugin_error", errHandle))
		return 1
	}
	writeABIResponse(response, raw)
	return 0
}

// validateABIBuffer runs before C.GoBytes can narrow size_t to a signed C.int.
func validateABIBuffer(hasPointer bool, length, limit uint64) error {
	if length > limit || length > math.MaxInt32 {
		return fmt.Errorf("buffer exceeds size limit")
	}
	if length > 0 && !hasPointer {
		return fmt.Errorf("non-empty buffer requires a pointer")
	}
	return nil
}

//export ZenPluginFree
func ZenPluginFree(ptr unsafe.Pointer, len C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export ZenPluginShutdown
func ZenPluginShutdown() {
	abiState.Lock()
	abiState.closing = true
	abiState.plugin = nil
	cancel := abiState.cancel
	abiState.Unlock()
	if cancel != nil {
		cancel()
	}
	abiState.Lock()
	// The host frees its C callback table after shutdown returns. Allow
	// closing callbacks while workers drain; never hold this mutex over C.
	for abiState.workers > 0 || abiState.callbacks > 0 {
		abiDrained.Wait()
	}
	abiState.host = nil
	abiState.ctx = nil
	abiState.cancel = nil
	abiState.Unlock()
}

func beginABIWorker() bool {
	abiState.Lock()
	defer abiState.Unlock()
	if abiState.closing {
		return false
	}
	abiState.workers++
	return true
}

func endABIWorker() {
	abiState.Lock()
	abiState.workers--
	abiDrained.Broadcast()
	abiState.Unlock()
}

func startABIWorker(work func()) bool {
	if !beginABIWorker() {
		return false
	}
	go func() {
		defer endABIWorker()
		work()
	}()
	return true
}

func handleABIMethod(ctx context.Context, method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return handleRegister(request)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return abiOKEnvelope(abiEmptyResponse{})
	}
	p, errPlugin := currentPlugin()
	if errPlugin != nil {
		return nil, errPlugin
	}
	switch method {
	case pluginabi.MethodExecutorIdentifier:
		return abiOKEnvelope(abiIdentifierResponse{Identifier: p.Identifier()})
	case pluginabi.MethodModelStatic:
		var req pluginapi.StaticModelRequest
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return nil, errDecode
		}
		resp, errCall := p.StaticModels(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)
	case pluginabi.MethodModelForAuth:
		var req pluginapi.AuthModelRequest
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return nil, errDecode
		}
		req.HTTPClient = nilClient(req.HTTPClient)
		resp, errCall := p.ModelsForAuth(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)
	case pluginabi.MethodModelRoute:
		var req pluginapi.ModelRouteRequest
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return nil, errDecode
		}
		resp, errCall := p.RouteModel(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)
	case pluginabi.MethodRequestTranslate:
		var req pluginapi.RequestTransformRequest
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return nil, errDecode
		}
		resp, errCall := p.TranslateRequest(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)
	case pluginabi.MethodResponseTranslate:
		var req pluginapi.ResponseTransformRequest
		if errDecode := json.Unmarshal(request, &req); errDecode != nil {
			return nil, errDecode
		}
		resp, errCall := p.TranslateResponse(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)
	case pluginabi.MethodExecutorExecute:
		var rpcReq abiExecutorRequest
		if errDecode := json.Unmarshal(request, &rpcReq); errDecode != nil {
			return nil, errDecode
		}
		req := rpcReq.ExecutorRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		resp, errCall := p.Execute(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)
	case pluginabi.MethodExecutorExecuteStream:
		var rpcReq abiExecutorRequest
		if errDecode := json.Unmarshal(request, &rpcReq); errDecode != nil {
			return nil, errDecode
		}
		req := rpcReq.ExecutorRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		streamCtx, cancel := context.WithCancel(ctx)
		resp, errCall := p.ExecuteStream(streamCtx, req)
		if errCall != nil {
			cancel()
			return nil, errCall
		}
		streamResp, errMarshal := marshalABIStreamResponse(streamCtx, cancel, rpcReq.StreamID, resp)
		if errMarshal != nil {
			return nil, errMarshal
		}
		return abiOKEnvelope(streamResp)
	case pluginabi.MethodExecutorCountTokens:
		var rpcReq abiExecutorRequest
		if errDecode := json.Unmarshal(request, &rpcReq); errDecode != nil {
			return nil, errDecode
		}
		req := rpcReq.ExecutorRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		resp, errCall := p.CountTokens(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)
	case pluginabi.MethodExecutorHTTPRequest:
		var rpcReq abiExecutorHTTPRequest
		if errDecode := json.Unmarshal(request, &rpcReq); errDecode != nil {
			return nil, errDecode
		}
		req := rpcReq.ExecutorHTTPRequest
		req.HTTPClient = abiHostHTTPClient{callbackID: rpcReq.HostCallbackID}
		resp, errCall := p.HttpRequest(ctx, req)
		return abiOKEnvelopeWithError(resp, errCall)
	case pluginabi.MethodRequestInterceptBefore, pluginabi.MethodRequestInterceptAfter:
		var proj interceptProjection
		if errDecode := json.Unmarshal(request, &proj); errDecode != nil {
			return nil, errDecode
		}
		req := pluginapi.RequestInterceptRequest{
			RequestID: proj.RequestID,
			Headers:   http.Header(proj.Headers),
			Metadata:  proj.Metadata,
		}
		var resp pluginapi.RequestInterceptResponse
		var errCall error
		if method == pluginabi.MethodRequestInterceptBefore {
			resp, errCall = p.InterceptRequestBeforeAuth(ctx, req)
		} else {
			resp, errCall = p.InterceptRequestAfterAuth(ctx, req)
		}
		return abiOKEnvelopeWithError(resp, errCall)
	default:
		return abiErrorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func handleRegister(request []byte) ([]byte, error) {
	var req abiLifecycleRequest
	if errDecode := json.Unmarshal(request, &req); errDecode != nil {
		return nil, errDecode
	}
	built, p, errBuild := plug.Build(req.ConfigYAML)
	if errBuild != nil {
		return nil, errBuild
	}
	if p == nil {
		return nil, fmt.Errorf("zen plugin registration returned invalid capabilities")
	}
	built.Metadata.Version = pluginVersion
	abiState.Lock()
	if abiState.closing {
		abiState.Unlock()
		return nil, fmt.Errorf("zen plugin is shutting down")
	}
	abiState.plugin = p
	abiState.Unlock()
	return abiOKEnvelope(abiRegistration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata:      built.Metadata,
		Capabilities: abiCapabilities{
			ModelProvider:         built.Capabilities.ModelProvider != nil,
			ModelRouter:           built.Capabilities.ModelRouter != nil,
			Executor:              built.Capabilities.Executor != nil,
			ExecutorModelScope:    built.Capabilities.ExecutorModelScope,
			ExecutorInputFormats:  append([]string(nil), built.Capabilities.ExecutorInputFormats...),
			ExecutorOutputFormats: append([]string(nil), built.Capabilities.ExecutorOutputFormats...),
			RequestTranslator:     built.Capabilities.RequestTranslator != nil,
			ResponseTranslator:    built.Capabilities.ResponseTranslator != nil,
			RequestInterceptor:    built.Capabilities.RequestInterceptor != nil,
		},
	})
}

func currentPlugin() (*plug.ZenPlugin, error) {
	abiState.RLock()
	defer abiState.RUnlock()
	if abiState.plugin == nil {
		return nil, fmt.Errorf("zen plugin is not registered")
	}
	return abiState.plugin, nil
}

func nilClient(c pluginapi.HostHTTPClient) pluginapi.HostHTTPClient { return c }

type abiHostHTTPClient struct {
	callbackID string
}

func (c abiHostHTTPClient) Do(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	return callHost[pluginapi.HTTPResponse](pluginabi.MethodHostHTTPDo, abiHostHTTPRequest{
		HTTPRequest:    req,
		HostCallbackID: c.callbackID,
	})
}

func (c abiHostHTTPClient) DoStream(ctx context.Context, req pluginapi.HTTPRequest) (pluginapi.HTTPStreamResponse, error) {
	// Reserve cleanup ownership before obtaining a Host stream ID. Shutdown
	// must not clear the callback table in the gap before the reader starts.
	if !beginABIWorker() {
		return pluginapi.HTTPStreamResponse{}, fmt.Errorf("zen plugin is shutting down")
	}
	transferred := false
	defer func() {
		if !transferred {
			endABIWorker()
		}
	}()
	resp, errCall := callHost[abiHostHTTPStreamResponse](pluginabi.MethodHostHTTPDoStream, abiHostHTTPRequest{
		HTTPRequest:    req,
		HostCallbackID: c.callbackID,
	})
	if errCall != nil {
		return pluginapi.HTTPStreamResponse{}, errCall
	}
	if resp.StreamID != "" {
		chunks := make(chan pluginapi.HTTPStreamChunk)
		transferred = true
		go func() {
			defer endABIWorker()
			readHostHTTPStream(ctx, resp.StreamID, chunks)
		}()
		return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Chunks: chunks}, nil
	}
	chunks := make(chan pluginapi.HTTPStreamChunk, len(resp.Chunks))
	for _, chunk := range resp.Chunks {
		chunks <- chunk
	}
	close(chunks)
	return pluginapi.HTTPStreamResponse{StatusCode: resp.StatusCode, Headers: resp.Headers, Chunks: chunks}, nil
}

func readHostHTTPStream(ctx context.Context, streamID string, out chan<- pluginapi.HTTPStreamChunk) {
	defer close(out)
	var once sync.Once
	closeStream := func() { once.Do(func() { closeHostHTTPStream(streamID) }) }
	// Close the upstream bridge even when cancellation happens during a
	// blocking host read, rather than waiting for its next payload.
	stop := context.AfterFunc(ctx, closeStream)
	defer func() {
		stop()
		closeStream()
	}()
	send := func(chunk pluginapi.HTTPStreamChunk) bool {
		select {
		case <-ctx.Done():
			return false
		case out <- chunk:
			return true
		}
	}
	for ctx.Err() == nil {
		resp, errRead := callHost[abiHostHTTPStreamReadResponse](pluginabi.MethodHostHTTPStreamRead, abiHostHTTPStreamReadRequest{StreamID: streamID})
		if errRead != nil {
			send(pluginapi.HTTPStreamChunk{Err: errRead})
			return
		}
		if resp.Error != "" {
			send(pluginapi.HTTPStreamChunk{Err: errors.New(resp.Error)})
			return
		}
		if len(resp.Payload) > 0 && !send(pluginapi.HTTPStreamChunk{Payload: resp.Payload}) {
			return
		}
		if resp.Done {
			return
		}
	}
}

func closeHostHTTPStream(streamID string) {
	_, _ = callHost[abiEmptyResponse](pluginabi.MethodHostHTTPStreamClose, abiHostHTTPStreamCloseRequest{StreamID: streamID})
}

func marshalABIStreamResponse(ctx context.Context, cancel context.CancelFunc, streamID string, resp pluginapi.ExecutorStreamResponse) (abiExecutorStreamResponse, error) {
	if resp.Chunks == nil {
		cancel()
		return abiExecutorStreamResponse{}, fmt.Errorf("executor returned no stream")
	}
	if streamID == "" {
		defer cancel()
		chunks := make([]pluginapi.ExecutorStreamChunk, 0)
		for {
			select {
			case <-ctx.Done():
				return abiExecutorStreamResponse{}, ctx.Err()
			case chunk, ok := <-resp.Chunks:
				if !ok {
					return abiExecutorStreamResponse{Headers: resp.Headers, Chunks: chunks}, nil
				}
				if chunk.Err != nil {
					return abiExecutorStreamResponse{}, chunk.Err
				}
				chunks = append(chunks, chunk)
			}
		}
	}
	if !startABIWorker(func() {
		defer cancel()
		pumpABIStream(ctx, streamID, resp.Chunks)
	}) {
		cancel()
		return abiExecutorStreamResponse{}, fmt.Errorf("zen plugin is shutting down")
	}
	return abiExecutorStreamResponse{Headers: resp.Headers}, nil
}

func pumpABIStream(ctx context.Context, streamID string, chunks <-chan pluginapi.ExecutorStreamChunk) {
	errorMessage := ""
	var once sync.Once
	closeStream := func(message string) {
		once.Do(func() {
			_, _ = callHost[abiEmptyResponse](pluginabi.MethodHostStreamClose, abiHostStreamCloseRequest{StreamID: streamID, Error: message})
		})
	}
	stop := context.AfterFunc(ctx, func() { closeStream(ctx.Err().Error()) })
	defer func() {
		stop()
		closeStream(errorMessage)
	}()
	for {
		select {
		case <-ctx.Done():
			if errCtx := ctx.Err(); errCtx != nil {
				errorMessage = errCtx.Error()
			}
			return
		case chunk, ok := <-chunks:
			if !ok {
				return
			}
			if chunk.Err != nil {
				errorMessage = chunk.Err.Error()
				return
			}
			if len(chunk.Payload) == 0 {
				continue
			}
			_, errCall := callHost[abiEmptyResponse](pluginabi.MethodHostStreamEmit, abiHostStreamEmitRequest{
				StreamID: streamID,
				Payload:  append([]byte(nil), chunk.Payload...),
			})
			if errCall != nil {
				errorMessage = errCall.Error()
				return
			}
		}
	}
}

func callHost[T any](method string, request any) (T, error) {
	var zero T
	abiState.Lock()
	host := abiState.host
	closingCall := method == pluginabi.MethodHostHTTPStreamClose || method == pluginabi.MethodHostStreamClose
	if host == nil || host.call == nil || (abiState.closing && !closingCall) {
		abiState.Unlock()
		return zero, fmt.Errorf("host callback is unavailable")
	}
	abiState.callbacks++
	abiState.Unlock()
	defer func() {
		abiState.Lock()
		abiState.callbacks--
		abiDrained.Broadcast()
		abiState.Unlock()
	}()
	rawRequest, errMarshal := json.Marshal(request)
	if errMarshal != nil {
		return zero, errMarshal
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var requestPtr *C.uint8_t
	if len(rawRequest) > 0 {
		requestPtr = (*C.uint8_t)(unsafe.Pointer(&rawRequest[0]))
	}
	var resp C.cliproxy_buffer
	code := C.zen_call_host(host, cMethod, requestPtr, C.size_t(len(rawRequest)), &resp)
	if resp.ptr != nil {
		defer C.zen_free_host_buffer(host, resp.ptr, resp.len)
	}
	if code != 0 {
		return zero, fmt.Errorf("host callback %s failed with code %d", method, int(code))
	}
	if err := validateABIBuffer(resp.ptr != nil, uint64(resp.len), math.MaxInt32); err != nil {
		return zero, fmt.Errorf("host callback %s: %w", method, err)
	}
	rawResp := C.GoBytes(resp.ptr, C.int(resp.len))
	var envelope pluginabi.Envelope
	if errDecode := json.Unmarshal(rawResp, &envelope); errDecode != nil {
		return zero, errDecode
	}
	if !envelope.OK {
		if envelope.Error != nil {
			return zero, fmt.Errorf("%s", envelope.Error.Message)
		}
		return zero, fmt.Errorf("host callback %s failed", method)
	}
	var out T
	if len(envelope.Result) == 0 {
		return out, nil
	}
	if errDecode := json.Unmarshal(envelope.Result, &out); errDecode != nil {
		return zero, errDecode
	}
	return out, nil
}

func abiOKEnvelope(v any) ([]byte, error) {
	result, errMarshal := json.Marshal(v)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}

func abiOKEnvelopeWithError(v any, err error) ([]byte, error) {
	if err != nil {
		return abiErrorEnvelopeFromError("plugin_error", err), nil
	}
	return abiOKEnvelope(v)
}

func abiErrorEnvelopeFromError(code string, err error) []byte {
	if err == nil {
		return abiErrorEnvelope(code, "")
	}
	httpStatus := 0
	var statusProvider interface{ StatusCode() int }
	if errors.As(err, &statusProvider) {
		httpStatus = statusProvider.StatusCode()
	}
	return abiErrorEnvelopeWithStatus(code, err.Error(), httpStatus)
}

func abiErrorEnvelope(code string, message string) []byte {
	return abiErrorEnvelopeWithStatus(code, message, 0)
}

func abiErrorEnvelopeWithStatus(code string, message string, httpStatus int) []byte {
	raw, _ := json.Marshal(pluginabi.Envelope{
		OK: false,
		Error: &pluginabi.Error{
			Code:       code,
			Message:    message,
			HTTPStatus: httpStatus,
		},
	})
	return raw
}

func writeABIResponse(response *C.cliproxy_buffer, data []byte) {
	if response == nil {
		return
	}
	if len(data) == 0 {
		response.ptr = nil
		response.len = 0
		return
	}
	ptr := C.malloc(C.size_t(len(data)))
	if ptr == nil {
		response.ptr = nil
		response.len = 0
		return
	}
	C.memcpy(ptr, unsafe.Pointer(&data[0]), C.size_t(len(data)))
	response.ptr = ptr
	response.len = C.size_t(len(data))
}

func main() {}
