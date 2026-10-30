package main

/*
#include <stdint.h>
#include <stdlib.h>

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

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"
)

var runtimeMu sync.Mutex
var running *worker

type worker struct {
	e    *Engine
	stop chan struct{}
	done chan struct{}
}

func (w *worker) halt() { w.e.stopped.Store(true); close(w.stop); <-w.done; w.e.close() }
func start(c Config) (*worker, error) {
	e, err := newEngine(c, callHost)
	if err != nil {
		return nil, err
	}
	w := &worker{e: e, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(c.interval())
		defer ticker.Stop()
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			e.tick()
			select {
			case <-w.stop:
				return
			case <-ticker.C:
			}
		}
	}()
	return w, nil
}
func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if host == nil || plugin == nil || host.abi_version != 1 || host.call == nil || host.free_buffer == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = 1
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, n C.size_t, response *C.cliproxy_buffer) (code C.int) {
	if response == nil {
		return 1
	}
	response.ptr = nil
	response.len = 0
	defer func() {
		if recover() != nil {
			writeResponse(response, map[string]any{"ok": false, "error": map[string]string{"code": "internal_error", "message": "plugin internal error"}})
			code = 1
		}
	}()
	if method == nil {
		return 1
	}
	if uint64(n) > 8*1024*1024 {
		return 1
	}
	var b []byte
	if request != nil && n > 0 {
		b = C.GoBytes(unsafe.Pointer(request), C.int(n))
	}
	result, err := handleMethod(C.GoString(method), b)
	if err != nil {
		writeResponse(response, map[string]any{"ok": false, "error": map[string]string{"code": "plugin_error", "message": err.Error()}})
		return 1
	}
	writeResponse(response, map[string]any{"ok": true, "result": result})
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, n C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if running != nil {
		running.halt()
		running = nil
	}
}
func writeResponse(out *C.cliproxy_buffer, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	out.ptr = C.CBytes(b)
	out.len = C.size_t(len(b))
}
func callHost(method string, payload, out any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	m := C.CString(method)
	defer C.free(unsafe.Pointer(m))
	p := C.CBytes(b)
	defer C.free(p)
	var resp C.cliproxy_buffer
	code := C.call_host_api(m, (*C.uint8_t)(p), C.size_t(len(b)), &resp)
	if resp.ptr == nil {
		return errors.New("host callback unavailable")
	}
	defer C.free_host_buffer(resp.ptr, resp.len)
	if uint64(resp.len) > 16*1024*1024 {
		return errors.New("host response too large")
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(C.GoBytes(resp.ptr, C.int(resp.len)), &env) != nil || code != 0 || !env.OK {
		return fmt.Errorf("host callback failed: %s", method)
	}
	return json.Unmarshal(env.Result, out)
}
func registration() any {
	return map[string]any{
		"schema_version": 6, "metadata": map[string]any{"Name": "codex-autoreset", "Version": "0.2.0", "Author": "AutoReset", "GitHubRepository": "https://github.com/jpam87/CLIProxyApiCodexAutoReset", "ConfigFields": []map[string]any{
			{"Name": "dry_run", "Type": "boolean", "Description": "Only record due resets; do not consume."},
			{"Name": "minutes_before_expiry", "Type": "integer", "Description": "Minutes before expiry (default 10)."},
			{"Name": "poll_seconds", "Type": "integer", "Description": "Poll interval in seconds (default 15)."},
			{"Name": "request_timeout_seconds", "Type": "integer", "Description": "Upstream timeout in seconds (default 30)."},
			{"Name": "state_file", "Type": "string", "Description": "Absolute persistent journal path; one instance per file."},
			{"Name": "accounts", "Type": "array", "Description": "Allowed auth_index values; empty selects all enabled Codex accounts."},
		}}, "capabilities": map[string]bool{"management_api": true}}
}
func handleMethod(method string, b []byte) (any, error) {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var req struct {
			YAML   []byte `json:"config_yaml"`
			Schema uint32 `json:"schema_version"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			return nil, errors.New("invalid lifecycle request")
		}
		if req.Schema < 6 {
			return nil, errors.New("requires CLIProxyAPI JSON schema 6 or newer")
		}
		c, err := parseConfig(req.YAML)
		if err != nil {
			return nil, err
		}
		if running != nil {
			running.halt()
			running = nil
		}
		if c.Enabled {
			running, err = start(c)
			if err != nil {
				return nil, errors.New("cannot start worker: check state path, permissions, or existing instance")
			}
		}
		return registration(), nil
	case "plugin.quiesce", "plugin.shutdown":
		if running != nil {
			running.halt()
			running = nil
		}
		return map[string]any{}, nil
	case "management.register":
		return map[string]any{"resources": []map[string]string{{"Path": "/status", "Menu": "Codex Auto Reset", "Description": "Automatic Codex reset-credit status."}}}, nil
	case "management.handle":
		body := map[string]any{"enabled": running != nil, "version": "0.2.0"}
		if running != nil {
			body["dry_run"] = running.e.cfg.DryRun
			body["minutes_before_expiry"] = running.e.cfg.Minutes
			body["accounts"] = running.e.snapshot()
		}
		raw, _ := json.Marshal(body)
		return map[string]any{"StatusCode": 200, "Headers": map[string][]string{"Content-Type": {"application/json"}}, "Body": raw}, nil
	default:
		return nil, fmt.Errorf("unsupported method: %s", method)
	}
}
