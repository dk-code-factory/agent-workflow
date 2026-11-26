package clirunner

import (
	"encoding/json"
	"fmt"
)

// The pinned native builtin OpenAI descriptor ignores configured retry overrides.
// Use a private descriptor retaining its auth-dependent default routing, headers
// and auth requirement, with bounded selected-turn retries and no WS fallback.
// This is internal to selected skills, not an application provider selector.
const codexSkillProvider = "agent-workflow-skill"

func codexSkillRetryArgs() []string {
	values := []string{
		`model_provider="` + codexSkillProvider + `"`,
		`features.unbounded_connection_retries=false`,
		`features.responses_websockets=false`,
		`features.responses_websockets_v2=false`,
		`model_providers.` + codexSkillProvider + `.name="OpenAI"`,
		`model_providers.` + codexSkillProvider + `.wire_api="responses"`,
		`model_providers.` + codexSkillProvider + `.requires_openai_auth=true`,
		`model_providers.` + codexSkillProvider + `.request_max_retries=0`,
		`model_providers.` + codexSkillProvider + `.stream_max_retries=0`,
		`model_providers.` + codexSkillProvider + `.supports_websockets=false`,
		`model_providers.` + codexSkillProvider + `.supports_standalone_web_search=true`,
		`model_providers.` + codexSkillProvider + `.http_headers.version="0.167.1"`,
		`model_providers.` + codexSkillProvider + `.env_http_headers.OpenAI-Organization="OPENAI_ORGANIZATION"`,
		`model_providers.` + codexSkillProvider + `.env_http_headers.OpenAI-Project="OPENAI_PROJECT"`,
	}
	args := make([]string, 0, len(values)*2)
	for _, v := range values {
		args = append(args, "--config", v)
	}
	return args
}

func verifyCodexSkillRetryConfig(raw json.RawMessage, explicitCommand bool) error {
	var response struct {
		Config  map[string]any `json:"config"`
		Origins map[string]struct {
			Name struct {
				Type string `json:"type"`
			} `json:"name"`
		} `json:"origins"`
		Layers []struct {
			Name struct {
				Type string `json:"type"`
			} `json:"name"`
			Config         map[string]any `json:"config"`
			DisabledReason *string        `json:"disabledReason"`
		} `json:"layers"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return fmt.Errorf("selected-skill retry control readback is malformed")
	}
	cfg := response.Config
	features, _ := cfg["features"].(map[string]any)
	providers, _ := cfg["model_providers"].(map[string]any)
	provider, _ := providers[codexSkillProvider].(map[string]any)
	if cfg["model_provider"] != codexSkillProvider || provider["name"] != "OpenAI" || provider["supports_standalone_web_search"] != true || provider["request_max_retries"] != float64(0) || provider["stream_max_retries"] != float64(0) || provider["supports_websockets"] != false || provider["wire_api"] != "responses" || provider["requires_openai_auth"] != true {
		return fmt.Errorf("selected-skill effective retry/transport provider controls unavailable or changed")
	}
	allowed := map[string]bool{"name": true, "wire_api": true, "requires_openai_auth": true, "request_max_retries": true, "stream_max_retries": true, "supports_websockets": true, "supports_standalone_web_search": true, "http_headers": true, "env_http_headers": true}
	if explicitCommand {
		allowed["base_url"] = true
	} // An explicit application command can already replace the process.
	for k, v := range provider {
		if v != nil && !allowed[k] {
			return fmt.Errorf("selected-skill provider inherited unadmitted control %s", k)
		}
	}
	if endpoint := provider["base_url"]; endpoint != nil {
		origin := response.Origins["model_providers."+codexSkillProvider+".base_url"]
		matched := false
		for _, layer := range response.Layers {
			ps, _ := layer.Config["model_providers"].(map[string]any)
			p, _ := ps[codexSkillProvider].(map[string]any)
			if layer.Name.Type == "sessionFlags" && layer.DisabledReason == nil && p["base_url"] == endpoint {
				matched = true
			}
		}
		if !explicitCommand || origin.Name.Type != "sessionFlags" || !matched {
			return fmt.Errorf("selected-skill provider endpoint was not supplied by the explicit application command")
		}
	}
	headers, _ := provider["http_headers"].(map[string]any)
	envHeaders, _ := provider["env_http_headers"].(map[string]any)
	if len(headers) != 1 || headers["version"] != "0.167.1" || len(envHeaders) != 2 || envHeaders["OpenAI-Organization"] != "OPENAI_ORGANIZATION" || envHeaders["OpenAI-Project"] != "OPENAI_PROJECT" {
		return fmt.Errorf("selected-skill provider routing headers changed")
	}
	for _, k := range []string{"unbounded_connection_retries", "responses_websockets", "responses_websockets_v2"} {
		if features[k] != false {
			return fmt.Errorf("selected-skill effective retry/transport feature %s unavailable or changed", k)
		}
	}
	return nil
}
