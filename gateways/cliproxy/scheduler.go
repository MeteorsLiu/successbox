package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// The scheduler owns a snapshot of token-to-account bindings. Values identify
// an account by its configured base_url; CLIProxyAPI owns credentials, model
// eligibility and cooldowns, and only its available candidates may be selected.
type scheduler struct {
	bindings atomic.Pointer[map[string]map[string]string]
}

func (s *scheduler) configure(raw []byte) error {
	// An invalid replacement must not keep authorizing a revoked token.
	s.bindings.Store(nil)
	var request struct {
		ConfigYAML []byte `json:"config_yaml"`
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return errors.New("invalid plugin configuration request")
	}
	var config struct {
		Bindings map[string]map[string]string `yaml:"bindings"`
	}
	if err := yaml.Unmarshal(request.ConfigYAML, &config); err != nil {
		// YAML diagnostics may contain the token used as a map key.
		return errors.New("invalid token binding configuration")
	}
	bindings := make(map[string]map[string]string, len(config.Bindings))
	for token, accounts := range config.Bindings {
		if token == "" || token != strings.TrimSpace(token) {
			return errors.New("binding token must be nonempty without surrounding whitespace")
		}
		for provider, authID := range accounts {
			if provider == "" || provider != strings.TrimSpace(provider) || authID == "" || authID != strings.TrimSpace(authID) {
				return errors.New("binding provider and account selector must be nonempty without surrounding whitespace")
			}
		}
		// CLIProxyAPI derives caller_scope from the principal that passed its
		// access check. Using it keeps conflicting Authorization/X-Api-Key
		// headers from selecting a different identity in the scheduler.
		sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + token))
		bindings[hex.EncodeToString(sum[:])] = accounts
	}
	s.bindings.Store(&bindings)
	return nil
}

func (s *scheduler) pick(raw []byte) ([]byte, error) {
	var request struct {
		Options struct {
			Metadata struct {
				CallerScope string `json:"caller_scope"`
			}
		}
		Candidates []struct {
			ID         string
			Provider   string
			Attributes map[string]string
		}
	}
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, errors.New("invalid scheduler request")
	}
	response := struct {
		Handled      bool
		AuthID       string `json:",omitempty"`
		Reject       bool
		RejectCode   string `json:",omitempty"`
		RejectReason string `json:",omitempty"`
	}{
		Handled:      true,
		Reject:       true,
		RejectCode:   "sandbox_binding_denied",
		RejectReason: "caller has no available bound credential",
	}
	if bindings := s.bindings.Load(); bindings != nil {
		accounts := (*bindings)[request.Options.Metadata.CallerScope]
		for _, candidate := range request.Candidates {
			// Mixed-provider requests have an empty request-level Provider.
			// Match the provider on the candidate, e.g. claude or codex.
			selector := accounts[candidate.Provider]
			baseURL, configIndex, hasIndex := strings.Cut(selector, "#")
			matches := candidate.ID == selector || candidate.Attributes["base_url"] == selector
			if hasIndex {
				matches = candidate.Attributes["base_url"] == baseURL && candidate.Attributes["config_index"] == configIndex
			}
			if selector != "" && matches {
				response.AuthID = candidate.ID
				response.Reject = false
				response.RejectCode, response.RejectReason = "", ""
				break
			}
		}
	}
	return json.Marshal(response)
}

func (s *scheduler) call(method string, raw []byte) ([]byte, error) {
	var result json.RawMessage
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := s.configure(raw); err != nil {
			return nil, err
		}
		// Across-priorities keeps an authorized lower-priority account in
		// the candidate set even when another caller's account ranks higher.
		result = json.RawMessage(`{"schema_version":6,"metadata":{"Name":"successbox","Version":"0.1.0","Author":"MeteorsLiu","GitHubRepository":"https://github.com/MeteorsLiu/successbox","ConfigFields":[{"Name":"bindings","Type":"object","Description":"Client token to provider and configured upstream base URL."}]},"capabilities":{"scheduler":true,"scheduler_across_priorities":true,"request_interceptor":true}}`)
	case "request.intercept_after":
		result = json.RawMessage(`{}`)
	case "request.intercept_before":
		var request struct {
			SourceFormat string
			Body         []byte
			Metadata     map[string]any
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, errors.New("invalid request interception envelope")
		}
		result = json.RawMessage(`{}`)
		bindings := s.bindings.Load()
		caller, _ := request.Metadata["caller_scope"].(string)
		if bindings == nil || len((*bindings)[caller]) == 0 {
			break
		}
		if request.SourceFormat != "claude" && request.SourceFormat != "openai-response" {
			break
		}
		var body map[string]json.RawMessage
		if json.Unmarshal(request.Body, &body) != nil || body == nil {
			return nil, errors.New("invalid model request")
		}
		var input []json.RawMessage
		lite := false
		policyIndex := 0
		if request.SourceFormat == "openai-response" && json.Unmarshal(body["input"], &input) == nil {
			// This selects the insertion location, never whether to inject.
			// Responses Lite carries instructions in developer input messages.
			for i, item := range input {
				var declaration struct {
					Type, Role string
				}
				if json.Unmarshal(item, &declaration) == nil && declaration.Type == "additional_tools" && declaration.Role == "developer" {
					lite, policyIndex = true, i+1
				}
			}
		}
		const instructions = "Successbox MCP rules: Before filling an MCP argument, check whether it is an API key, password, token, client_secret or other private credential. For a sensitive field use exactly $PRIVACY_PROTECT_<field_name>, replacing <field_name> with that argument's lowercase letters, digits and underscores; for example api_key becomes $PRIVACY_PROTECT_api_key. This is a host gateway placeholder, not the actual credential and not a shell variable to expand. The MCP gateway obtains the user's saved value or asks the frontend for it, then replaces the parameter immediately before the upstream call. Never request a real credential in this conversation. Ordinary URLs, repository names and query text are not credential placeholders. To add an MCP, find and call request_mcp_install using the client's supported tool discovery and execution mechanism, with its name, transport, endpoint, requested_tools and credential_fields. Supply no secret or mcp_id. The user approves and enters credentials in the frontend. Approval alone does not make a tool callable: use only tools actually available through the client. The MCP client refreshes its catalog; do not fabricate tool names or call tools/list as a model tool."
		const begin, end = "<successbox_mcp>\n", "\n</successbox_mcp>"
		policy := begin + instructions + end
		field := "instructions"
		if request.SourceFormat == "claude" {
			field = "system"
		}
		previous := body[field]
		var text string
		if lite {
			block, _ := json.Marshal(map[string]any{"type": "message", "role": "developer", "content": []map[string]string{{"type": "input_text", "text": policy}}})
			already, replaced := false, false
			for i, item := range input {
				var message struct {
					Type, Role string
					Content    []struct{ Type, Text string }
				}
				if json.Unmarshal(item, &message) != nil || message.Type != "message" || message.Role != "developer" || len(message.Content) != 1 || message.Content[0].Type != "input_text" {
					continue
				}
				text := message.Content[0].Text
				if text == policy {
					already = true
					break
				}
				if strings.HasPrefix(text, begin) && strings.HasSuffix(text, end) {
					input[i], replaced = block, true
				}
			}
			if already {
				break
			}
			if !replaced {
				input = append(input, nil)
				copy(input[policyIndex+1:], input[policyIndex:])
				input[policyIndex] = block
			}
			body["input"], _ = json.Marshal(input)
		} else if len(previous) == 0 || string(previous) == "null" || json.Unmarshal(previous, &text) == nil {
			if strings.Contains(text, policy) {
				break
			}
			replaced := false
			if before, rest, found := strings.Cut(text, begin); found {
				if _, after, complete := strings.Cut(rest, end); complete {
					text = before + policy + after
					replaced = true
				}
			}
			if !replaced {
				if text != "" {
					text += "\n\n"
				}
				text += policy
			}
			body[field], _ = json.Marshal(text)
		} else if request.SourceFormat == "claude" {
			var blocks []json.RawMessage
			if json.Unmarshal(previous, &blocks) != nil {
				return nil, errors.New("invalid Claude system instructions")
			}
			already, replaced := false, false
			for i, block := range blocks {
				var v struct{ Text string }
				_ = json.Unmarshal(block, &v)
				if v.Text == policy {
					already = true
					break
				}
				if strings.HasPrefix(v.Text, begin) && strings.HasSuffix(v.Text, end) {
					blocks[i], _ = json.Marshal(map[string]string{"type": "text", "text": policy})
					replaced = true
				}
			}
			if already {
				break
			}
			if !replaced {
				block, _ := json.Marshal(map[string]string{"type": "text", "text": policy})
				blocks = append(blocks, block)
			}
			body[field], _ = json.Marshal(blocks)
		} else {
			return nil, errors.New("invalid Responses instructions")
		}
		patched, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("encode model request failed")
		}
		result, err = json.Marshal(struct{ Body []byte }{patched})
		if err != nil {
			return nil, err
		}
	case "scheduler.pick":
		var err error
		result, err = s.pick(raw)
		if err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unsupported plugin method")
	}
	return json.Marshal(struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}{true, result})
}
