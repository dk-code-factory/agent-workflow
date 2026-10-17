package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// A real provider can call this local fixture. The first external effect occurs,
// then its HTTP response is lost. The broker must not dispatch another effect.
func uncertainMCPFixture() (*httptest.Server, *atomic.Int32) {
	server := mcp.NewServer(&mcp.Implementation{Name: "conformance-uncertain", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "effect", Description: "Conformance-only external effect. Call once with an empty object.", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "unreachable"}}}, nil
	})
	protocol := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	count := &atomic.Int32{}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			if err != nil {
				w.WriteHeader(400)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			var frame struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(data, &frame)
			if frame.Method == "tools/call" {
				count.Add(1)
				connection, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = connection.Close()
				}
				return
			}
		}
		protocol.ServeHTTP(w, r)
	}))
	return endpoint, count
}
