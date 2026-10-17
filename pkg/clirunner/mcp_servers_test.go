package clirunner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dkropachev/agent-workflow/pkg/workflows"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestMCPStdioTransportUsesBrokerEnvironment(t *testing.T) {
	if os.Getenv("AW_MCP_STDIO_FIXTURE") == "selected-stdio-fixture" {
		server := mcp.NewServer(&mcp.Implementation{Name: "stdio-fixture", Version: "1"}, nil)
		server.AddTool(&mcp.Tool{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "STDIO_OK"}}}, nil
		})
		if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKFLOW_MCP_STDIO_SELECTOR", "selected-stdio-fixture")
	config := Config{MCPServers: map[string]MCPServer{"stdio": {Transport: "stdio", Command: self, Args: []string{"-test.run=^TestMCPStdioTransportUsesBrokerEnvironment$"}, Environment: map[string]string{"AW_MCP_STDIO_FIXTURE": "WORKFLOW_MCP_STDIO_SELECTOR"}, Timeout: "5s"}}, Agents: map[string]Agent{"a": {MCPGrants: []workflows.ToolGrant{{Server: "stdio", Tool: "lookup"}}}}}
	runner := &Runner{Config: config, Workspace: t.TempDir()}
	roles, err := runner.AdmittedRoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var admitted Config
	if err = json.Unmarshal(roles, &admitted); err != nil {
		t.Fatal(err)
	}
	result, err := runner.ExternalTool(context.Background(), admitted.ToolCatalog[0], json.RawMessage(`{}`))
	if err != nil || !strings.Contains(string(result), "STDIO_OK") {
		t.Fatal(string(result), err)
	}
	if strings.Contains(strings.Join(providerEnvironment(""), "\n"), "selected-stdio-fixture") {
		t.Fatal("server-only environment escaped into provider")
	}
}

func TestMCPHTTPGrantsPinnedCatalogAndCredentialIsolation(t *testing.T) {
	t.Setenv("MCP_FIXTURE_HEADER", "private-server-token")
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	add := func(name, description string) {
		server.AddTool(&mcp.Tool{Name: name, Description: description, InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "looked up"}}}, nil
		})
	}
	add("lookup", "Pinned description")
	add("ungranted", "Never exposed")
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "private-server-token" {
			t.Error("broker credential reference not applied")
		}
		handler.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	config := Config{MCPServers: map[string]MCPServer{"fixture": {Transport: "streamable-http", Endpoint: endpoint.URL, Headers: map[string]string{"Authorization": "MCP_FIXTURE_HEADER"}}}, Agents: map[string]Agent{"a": {Provider: "claude-sdk", MCPGrants: []workflows.ToolGrant{{Server: "fixture", Tool: "lookup"}}}}}
	runner := &Runner{Config: config, Workspace: t.TempDir()}
	roles, err := runner.AdmittedRoles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(roles), "private-server-token") || strings.Contains(string(roles), "Never exposed") {
		t.Fatal("admitted catalog leaked secret or ungranted tool")
	}
	var pinned Config
	if err = json.Unmarshal(roles, &pinned); err != nil {
		t.Fatal(err)
	}
	if len(pinned.ToolCatalog) != 1 || pinned.ToolCatalog[0].Fingerprint == "" {
		t.Fatal("catalog not pinned", pinned.ToolCatalog)
	}
	runner.Config = pinned
	if err = runner.VerifyToolCatalog(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := runner.ExternalTool(context.Background(), pinned.ToolCatalog[0], json.RawMessage(`{"query":"x"}`))
	if err != nil || !strings.Contains(string(result), "looked up") {
		t.Fatal(string(result), err)
	}
	providerEnv := strings.Join(providerEnvironment(""), "\n")
	if strings.Contains(providerEnv, "private-server-token") {
		t.Fatal("external credential forwarded to provider")
	}
	add("new_ungranted", "New tools must not silently be authorized")
	if err = runner.VerifyToolCatalog(context.Background()); err != nil {
		t.Fatal("ungranted addition changed pinned catalog", err)
	}
	add("lookup", "Changed description")
	if err = runner.VerifyToolCatalog(context.Background()); err == nil {
		t.Fatal("incompatible catalog accepted on resume")
	}
}

func TestMCPConfigRequiresExactGrantsAndReferences(t *testing.T) {
	base := Config{MCPServers: map[string]MCPServer{"fixture": {Transport: "stdio", Command: "fixture-server"}}, Agents: map[string]Agent{"a": {MCPGrants: []workflows.ToolGrant{{Server: "fixture", Tool: "*"}}}}}
	if err := base.validateMCP(); err == nil {
		t.Fatal("wildcard grant accepted")
	}
	base.Agents["a"] = Agent{MCPGrants: []workflows.ToolGrant{{Server: "missing", Tool: "lookup"}}}
	if err := base.validateMCP(); err == nil {
		t.Fatal("unknown server accepted")
	}
	base.Agents["a"] = Agent{MCPGrants: []workflows.ToolGrant{{Server: "fixture", Tool: "lookup"}}}
	if err := base.validateMCP(); err != nil {
		t.Fatal(err)
	}
}
