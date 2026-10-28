package clirunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/dk-code-factory/agent-workflow/pkg/runtimeexec"
	"github.com/dk-code-factory/agent-workflow/pkg/workflows"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type MCPServer struct {
	Transport   string            `yaml:"transport"`
	Command     string            `yaml:"command"`
	Args        []string          `yaml:"args"`
	Endpoint    string            `yaml:"endpoint"`
	Timeout     string            `yaml:"timeout"`
	Environment map[string]string `yaml:"environment-env"`
	Headers     map[string]string `yaml:"headers-env"`
}

func (c Config) validateMCP() error {
	for name, s := range c.MCPServers {
		if !roleIDPattern.MatchString(name) || name == "engine" {
			return fmt.Errorf("invalid or reserved MCP server name %q", name)
		}
		if _, err := serverTimeout(s); err != nil {
			return err
		}
		switch s.Transport {
		case "stdio":
			if s.Command == "" || s.Endpoint != "" || len(s.Headers) > 0 {
				return fmt.Errorf("MCP server %s: stdio requires command and no HTTP settings", name)
			}
		case "streamable-http":
			u, err := url.Parse(s.Endpoint)
			if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || (u.Scheme != "https" && u.Scheme != "http") || s.Command != "" || len(s.Args) > 0 || len(s.Environment) > 0 {
				return fmt.Errorf("MCP server %s: invalid HTTP transport settings", name)
			}
		default:
			return fmt.Errorf("MCP server %s: transport must be stdio or streamable-http", name)
		}
		for key, ref := range s.Environment {
			if key == "" || strings.ContainsAny(key, "=\x00") || !roleIDPattern.MatchString(ref) {
				return fmt.Errorf("invalid MCP environment credential reference")
			}
		}
		for key, ref := range s.Headers {
			if key == "" || strings.ContainsAny(key, "\r\n:") || !roleIDPattern.MatchString(ref) {
				return fmt.Errorf("invalid MCP header credential reference")
			}
		}
	}
	for id, role := range c.Agents {
		seen := map[workflows.ToolGrant]bool{}
		for _, g := range role.MCPGrants {
			if g.Server == "" || g.Tool == "" || strings.ContainsAny(g.Server+g.Tool, "*?\x00") || seen[g] {
				return fmt.Errorf("role %s requires unique exact MCP server/tool grants", id)
			}
			seen[g] = true
			if g.Server != "engine" {
				if _, ok := c.MCPServers[g.Server]; !ok {
					return fmt.Errorf("role %s references unknown MCP server %s", id, g.Server)
				}
			}
		}
	}
	return nil
}

func serverTimeout(s MCPServer) (time.Duration, error) {
	if s.Timeout == "" {
		return 30 * time.Second, nil
	}
	d, err := time.ParseDuration(s.Timeout)
	if err != nil || d <= 0 || d > 30*time.Minute {
		return 0, fmt.Errorf("MCP timeout must be positive and at most 30m")
	}
	return d, nil
}

type credentialTransport struct {
	base    http.RoundTripper
	headers http.Header
}

type boundedHTTPBody struct {
	io.Reader
	io.Closer
}

func (t credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	for name, values := range t.headers {
		clone.Header[name] = append([]string(nil), values...)
	}
	response, err := t.base.RoundTrip(clone)
	if err == nil && response.Body != nil {
		response.Body = boundedHTTPBody{Reader: io.LimitReader(response.Body, maxOutput), Closer: response.Body}
	}
	return response, err
}

func (r *Runner) mcpSession(ctx context.Context, name string) (*mcp.ClientSession, func(), error) {
	s, ok := r.Config.MCPServers[name]
	if !ok {
		return nil, nil, fmt.Errorf("unknown MCP server")
	}
	var transport mcp.Transport
	cleanup := func() {}
	switch s.Transport {
	case "stdio":
		cmd := exec.CommandContext(ctx, s.Command, s.Args...)
		cmd.Dir = r.Workspace
		cmd.Env = providerEnvironment("")
		cmd.Stderr = io.Discard
		for key, ref := range s.Environment {
			value, ok := os.LookupEnv(ref)
			if !ok {
				return nil, nil, fmt.Errorf("MCP credential reference %s unavailable", ref)
			}
			cmd.Env = append(cmd.Env, key+"="+value)
		}
		var err error
		cleanup, err = runtimeexec.SupervisePolicy(ctx, cmd, r.Config.Execution.policy())
		if err != nil {
			return nil, nil, err
		}
		transport = &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}
	case "streamable-http":
		headers := make(http.Header)
		for key, ref := range s.Headers {
			value, ok := os.LookupEnv(ref)
			if !ok {
				return nil, nil, fmt.Errorf("MCP credential reference %s unavailable", ref)
			}
			if strings.ContainsAny(value, "\r\n") {
				return nil, nil, fmt.Errorf("MCP header credential contains invalid bytes")
			}
			headers.Set(key, value)
		}
		timeout, err := serverTimeout(s)
		if err != nil {
			return nil, nil, err
		}
		client := &http.Client{Timeout: timeout, Transport: credentialTransport{base: http.DefaultTransport, headers: headers}, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("MCP endpoint redirects are disabled") }}
		transport = &mcp.StreamableClientTransport{Endpoint: s.Endpoint, HTTPClient: client, MaxRetries: -1}
	default:
		return nil, nil, fmt.Errorf("unsupported MCP transport")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "agent-workflow-broker", Version: "1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return session, func() { _ = session.Close(); cleanup() }, nil
}

func fingerprintTool(d workflows.ToolDefinition) (workflows.ToolDefinition, error) {
	d.Fingerprint = ""
	var schema any
	decoder := json.NewDecoder(bytes.NewReader(d.InputSchema))
	decoder.UseNumber()
	if err := decoder.Decode(&schema); err != nil {
		return d, err
	}
	object, ok := schema.(map[string]any)
	if !ok || object["type"] != "object" {
		return d, fmt.Errorf("MCP tool %s/%s requires an object input schema", d.Server, d.Name)
	}
	if version, ok := object["$schema"].(string); ok && version != "https://json-schema.org/draft/2020-12/schema" {
		return d, fmt.Errorf("MCP tool %s/%s uses an unsupported schema dialect", d.Server, d.Name)
	}
	var validation jsonschema.Schema
	if err := json.Unmarshal(d.InputSchema, &validation); err != nil {
		return d, err
	}
	if _, err := validation.Resolve(nil); err != nil {
		return d, fmt.Errorf("MCP tool %s/%s has an unsupported schema: %w", d.Server, d.Name, err)
	}
	d.InputSchema, _ = json.Marshal(schema)
	b, err := json.Marshal(d)
	if err != nil {
		return d, err
	}
	h := sha256.Sum256(b)
	d.Fingerprint = hex.EncodeToString(h[:])
	return d, nil
}

func (r *Runner) discoverMCP(ctx context.Context) ([]workflows.ToolDefinition, error) {
	servers := map[string]bool{}
	for _, role := range r.Config.Agents {
		for _, g := range role.MCPGrants {
			servers[g.Server] = true
		}
	}
	var definitions []workflows.ToolDefinition
	for name := range servers {
		if name == "engine" {
			definitions = append(definitions, workflows.EngineTools()...)
			continue
		}
		timeout, err := serverTimeout(r.Config.MCPServers[name])
		if err != nil {
			return nil, err
		}
		serverCtx, cancel := context.WithTimeout(ctx, timeout)
		session, closeSession, err := r.mcpSession(serverCtx, name)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("MCP server %s unavailable: %w", name, err)
		}
		var catalog []workflows.ToolDefinition
		cursor := ""
		seen := map[string]bool{}
		for pages := 0; ; pages++ {
			if pages >= 100 {
				err = fmt.Errorf("MCP catalog pagination limit")
				break
			}
			var result *mcp.ListToolsResult
			result, err = session.ListTools(serverCtx, &mcp.ListToolsParams{Cursor: cursor})
			if err != nil {
				break
			}
			for _, tool := range result.Tools {
				schema, marshalErr := json.Marshal(tool.InputSchema)
				if marshalErr != nil {
					err = marshalErr
					break
				}
				if len(schema) > 1<<20 || len(catalog) >= 1024 {
					err = fmt.Errorf("MCP catalog exceeds limit")
					break
				}
				catalog = append(catalog, workflows.ToolDefinition{Server: name, Name: tool.Name, Description: tool.Description, InputSchema: schema})
			}
			if err != nil || result.NextCursor == "" {
				break
			}
			if seen[result.NextCursor] {
				err = fmt.Errorf("repeated MCP catalog cursor")
				break
			}
			seen[result.NextCursor] = true
			cursor = result.NextCursor
		}
		closeSession()
		cancel()
		if err != nil {
			return nil, err
		}
		definitions = append(definitions, catalog...)
	}
	// Pin only explicitly granted tools. An ungranted new upstream tool never
	// changes the provider-visible catalog or expands access.
	out := make([]workflows.ToolDefinition, 0)
	catalogBytes := 0
	seen := map[workflows.ToolGrant]bool{}
	for _, d := range definitions {
		g := workflows.ToolGrant{Server: d.Server, Tool: d.Name}
		granted := false
		for _, role := range r.Config.Agents {
			for _, grant := range role.MCPGrants {
				if grant == g {
					granted = true
				}
			}
		}
		if !granted {
			continue
		}
		if seen[g] {
			return nil, fmt.Errorf("duplicate MCP tool identity")
		}
		seen[g] = true
		pinned, err := fingerprintTool(d)
		if err != nil {
			return nil, err
		}
		catalogBytes += len(pinned.InputSchema) + len(pinned.Description) + len(pinned.Name) + len(pinned.Server)
		if catalogBytes > 4<<20 {
			return nil, fmt.Errorf("granted MCP catalog exceeds 4 MiB")
		}
		out = append(out, pinned)
	}
	for _, role := range r.Config.Agents {
		for _, g := range role.MCPGrants {
			if !seen[g] {
				return nil, fmt.Errorf("granted MCP tool %s/%s is unavailable", g.Server, g.Tool)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Server == out[j].Server {
			return out[i].Name < out[j].Name
		}
		return out[i].Server < out[j].Server
	})
	return out, nil
}

func (r *Runner) AdmittedRoles(ctx context.Context) (roles json.RawMessage, err error) {
	defer func() { err = workflows.RedactError(err, r.SecretValues()) }()
	cfg, err := r.Config.PinRuntimes(ctx)
	if err != nil {
		return nil, err
	}
	pinned := &Runner{Config: cfg, Workspace: r.Workspace}
	catalog, err := pinned.discoverMCP(ctx)
	if err != nil {
		return nil, err
	}
	cfg.ToolCatalog = catalog
	encoded, _ := json.Marshal(catalog)
	hash := sha256.Sum256(encoded)
	cfg.CatalogFingerprint = hex.EncodeToString(hash[:])
	return json.Marshal(cfg)
}

func (r *Runner) VerifyToolCatalog(ctx context.Context) error {
	current, err := r.discoverMCP(ctx)
	if err != nil {
		return err
	}
	expected, _ := json.Marshal(r.Config.ToolCatalog)
	actual, _ := json.Marshal(current)
	hash := sha256.Sum256(actual)
	if r.Config.CatalogFingerprint != "" && r.Config.CatalogFingerprint != hex.EncodeToString(hash[:]) {
		return fmt.Errorf("admitted MCP catalog fingerprint changed")
	}
	if string(expected) != string(actual) {
		return fmt.Errorf("admitted MCP tool catalog changed; recovery requires compatible server schemas and descriptions")
	}
	return nil
}

func (r *Runner) ToolPolicy(role string) ([]workflows.ToolGrant, []workflows.ToolDefinition) {
	grants := append([]workflows.ToolGrant{}, r.Config.Agents[role].MCPGrants...)
	catalog := make([]workflows.ToolDefinition, 0)
	for _, d := range r.Config.ToolCatalog {
		for _, g := range grants {
			if d.Server == g.Server && d.Name == g.Tool {
				catalog = append(catalog, d)
				break
			}
		}
	}
	return grants, catalog
}

func (r *Runner) ExternalTool(ctx context.Context, definition workflows.ToolDefinition, args json.RawMessage) (json.RawMessage, error) {
	timeout, err := serverTimeout(r.Config.MCPServers[definition.Server])
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	session, closeSession, err := r.mcpSession(ctx, definition.Server)
	if err != nil {
		return nil, err
	}
	defer closeSession()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: definition.Name, Arguments: args})
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(result)
	if len(b) > maxOutput {
		return nil, fmt.Errorf("MCP result exceeds limit")
	}
	return b, err
}
