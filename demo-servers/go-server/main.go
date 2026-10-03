// mcpload demo target: official Go MCP SDK (go-sdk v1.8), stateless streamable HTTP.
//
// StreamableHTTPOptions.Stateless=true makes the handler accept the 2026-07-28
// stateless protocol (no initialize, per-request _meta, Mcp-Method/Mcp-Name headers,
// server/discover) and also serve legacy clients (initialize handshake is accepted but
// no Mcp-Session-Id is issued or checked). GET/DELETE /mcp return 405.
//
// Env: PORT (3000), FLAKY_RATE (0.1), BIG_BYTES (200000), JSON_RESPONSE (0), SERVER_NAME (stateless-2026).
//
// Version-skew targets (demo-servers/README.md, scenarios/version-skew.js) use this image as the newer build:
//
//	NEW_TOOL=1        also list `new_tool`, a tool the older build doesn't have
//	STATELESS_ONLY=1  the build dropped the stateful protocol: a request that is not 2026-07-28 or later
//	                  (initialize, or a Mcp-Protocol-Version older than 2026-07-28) gets HTTP 400 with
//	                  JSON-RPC -32022 UnsupportedProtocolVersion, data {supported: ["2026-07-28"], requested}
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	flakyRate  = envFloat("FLAKY_RATE", 0.1)
	bigBytes   = envInt("BIG_BYTES", 200000)
	bigText    = makeBigText(bigBytes)
	replica, _ = os.Hostname()

	serverName    = envStr("SERVER_NAME", "stateless-2026")
	newTool       = os.Getenv("NEW_TOOL") == "1"
	statelessOnly = os.Getenv("STATELESS_ONLY") == "1"
)

func envStr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envFloat(k string, d float64) float64 {
	if v, err := strconv.ParseFloat(os.Getenv(k), 64); err == nil {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v, err := strconv.Atoi(os.Getenv(k)); err == nil {
		return v
	}
	return d
}

func makeBigText(n int) string {
	line := "mcpload big payload 0123456789 abcdefghijklmnopqrstuvwxyz ABCDEFGHIJKLMNOPQRSTUVWXYZ\n"
	return strings.Repeat(line, n/len(line)+1)[:n]
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

type noArgs struct{}

type slowArgs struct {
	Ms *int `json:"ms,omitempty" jsonschema:"milliseconds to sleep (default 300)"`
}

type flakyArgs struct {
	Rate *float64 `json:"rate,omitempty" jsonschema:"probability of isError (default FLAKY_RATE)"`
}

type bigArgs struct {
	Bytes *int `json:"bytes,omitempty" jsonschema:"payload size in bytes (default BIG_BYTES)"`
}

type searchArgs struct {
	Query string `json:"query" jsonschema:"search query"`
	Limit *int   `json:"limit,omitempty" jsonschema:"number of results (default 5)"`
}

func newServer() *mcp.Server {
	var opts *mcp.ServerOptions
	if statelessOnly {
		// server/discover advertises only what this build still speaks.
		opts = &mcp.ServerOptions{SupportedProtocolVersions: []string{"2026-07-28"}}
	}
	s := mcp.NewServer(&mcp.Implementation{Name: serverName, Version: "0.1.0"}, opts)

	mcp.AddTool(s, &mcp.Tool{Name: "fast", Description: "Returns immediately."},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
			return text("ok"), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "slow", Description: "Sleeps for `ms` milliseconds (default 300) before returning."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a slowArgs) (*mcp.CallToolResult, any, error) {
			ms := 300
			if a.Ms != nil {
				ms = min(max(*a.Ms, 0), 120000)
			}
			select {
			case <-time.After(time.Duration(ms) * time.Millisecond):
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			}
			return text(fmt.Sprintf("slept %dms", ms)), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "flaky", Description: fmt.Sprintf("Returns isError:true with probability `rate` (default %g).", flakyRate)},
		func(ctx context.Context, _ *mcp.CallToolRequest, a flakyArgs) (*mcp.CallToolResult, any, error) {
			p := flakyRate
			if a.Rate != nil {
				p = *a.Rate
			}
			if rand.Float64() < p {
				r := text("flaky: simulated tool failure")
				r.IsError = true
				return r, nil, nil
			}
			return text("flaky: ok"), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "big", Description: fmt.Sprintf("Returns a large text payload (default %d bytes).", bigBytes)},
		func(ctx context.Context, _ *mcp.CallToolRequest, a bigArgs) (*mcp.CallToolResult, any, error) {
			if a.Bytes == nil || *a.Bytes == bigBytes {
				return text(bigText), nil, nil
			}
			return text(makeBigText(min(max(*a.Bytes, 1), 10_000_000))), nil, nil
		})

	mcp.AddTool(s, &mcp.Tool{Name: "search", Description: "Echoes the query with a short fake result list."},
		func(ctx context.Context, _ *mcp.CallToolRequest, a searchArgs) (*mcp.CallToolResult, any, error) {
			n := 5
			if a.Limit != nil {
				n = min(max(*a.Limit, 1), 50)
			}
			type result struct {
				Title string  `json:"title"`
				URL   string  `json:"url"`
				Score float64 `json:"score"`
			}
			res := make([]result, n)
			for i := range res {
				res[i] = result{
					Title: fmt.Sprintf("Result %d for %q", i+1, a.Query),
					URL:   fmt.Sprintf("https://example.com/search/%s/%d", url.PathEscape(a.Query), i+1),
					Score: float64(int((1-float64(i)/float64(n+1))*1000)) / 1000,
				}
			}
			b, _ := json.Marshal(map[string]any{"query": a.Query, "results": res})
			return text(string(b)), nil, nil
		})

	if newTool {
		mcp.AddTool(s, &mcp.Tool{Name: "new_tool", Description: "Only on the newer build (NEW_TOOL=1)."},
			func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, any, error) {
				return text("new_tool: ok from " + replica), nil, nil
			})
	}
	return s
}

// rejectLegacy (STATELESS_ONLY=1) answers every POST that does not use the 2026-07-28 (or later) protocol
// with a typed UnsupportedProtocolVersion error, as a build that dropped the stateful protocol would.
func rejectLegacy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		if err != nil {
			http.Error(w, "reading body: "+err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
			} `json:"params"`
		}
		_ = json.Unmarshal(body, &msg)
		v := r.Header.Get("Mcp-Protocol-Version")
		if msg.Method == "initialize" {
			v = msg.Params.ProtocolVersion
		} else if v >= "2026-07-28" {
			next.ServeHTTP(w, r)
			return
		}
		id := msg.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		resp, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{
			"code":    -32022,
			"message": fmt.Sprintf("Unsupported protocol version %q: this build only speaks 2026-07-28", v),
			"data":    map[string]any{"supported": []string{"2026-07-28"}, "requested": v},
		}})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(resp)
	})
}

func rssBytes() int64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 0
	}
	pages, _ := strconv.ParseInt(f[1], 10, 64)
	return pages * int64(os.Getpagesize())
}

func metrics(w http.ResponseWriter, _ *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE process_resident_memory_bytes gauge\nprocess_resident_memory_bytes %d\n", rssBytes())
	fmt.Fprintf(w, "# TYPE go_memstats_heap_alloc_bytes gauge\ngo_memstats_heap_alloc_bytes %d\n", m.HeapAlloc)
	fmt.Fprintf(w, "# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "# TYPE mcp_active_sessions gauge\nmcp_active_sessions 0\n")
}

func main() {
	server := newServer()
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: os.Getenv("JSON_RESPONSE") == "1",
	})

	mux := http.NewServeMux()
	if statelessOnly {
		mux.Handle("/mcp", rejectLegacy(handler))
	} else {
		mux.Handle("/mcp", handler)
	}
	mux.HandleFunc("GET /metrics", metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"name":%q,"replica":%q,"protocols":%q}`, serverName, replica, mcp.SupportedProtocolVersions())
	})

	addr := ":" + strconv.Itoa(envInt("PORT", 3000))
	log.Printf("%s (go-sdk) listening on %s, protocols %v, new_tool %v, stateless only %v", serverName, addr, mcp.SupportedProtocolVersions(), newTool, statelessOnly)
	withReplica := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Served-By", replica)
		mux.ServeHTTP(w, r)
	})
	log.Fatal(http.ListenAndServe(addr, withReplica))
}
