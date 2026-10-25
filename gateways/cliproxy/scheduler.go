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
		result = json.RawMessage(`{"schema_version":6,"metadata":{"Name":"successbox","Version":"0.1.0","Author":"MeteorsLiu","GitHubRepository":"https://github.com/MeteorsLiu/successbox","ConfigFields":[{"Name":"bindings","Type":"object","Description":"Client token to provider and configured upstream base URL."}]},"capabilities":{"scheduler":true,"scheduler_across_priorities":true}}`)
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
