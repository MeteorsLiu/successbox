package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestTokenBindings(t *testing.T) {
	var s scheduler
	configuration, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("bindings:\n  token-a:\n    claude: account-a\n    codex: codex-a\n  token-b:\n    claude: account-b\n")})
	if err := s.configure(configuration); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, token, candidates, want string
	}{
		{"A", "token-a", `[{"ID":"account-b","Provider":"claude","Attributes":{"base_url":"https://a","config_index":"1"}},{"ID":"account-a","Provider":"claude","Attributes":{"base_url":"https://a","config_index":"0"}}]`, "account-a"},
		{"B", "token-b", `[{"ID":"account-a","Provider":"claude","Attributes":{"base_url":"https://a","config_index":"0"}},{"ID":"account-b","Provider":"claude","Attributes":{"base_url":"https://a","config_index":"1"}}]`, "account-b"},
		{"codex", "token-a", `[{"ID":"codex-a","Provider":"codex","Attributes":{"base_url":"https://a","config_index":"0"}}]`, "codex-a"},
		{"unknown caller", "unknown", `[{"ID":"account-a","Provider":"claude","Attributes":{"base_url":"https://a"}}]`, ""},
		{"bound account unavailable", "token-a", `[{"ID":"account-b","Provider":"claude","Attributes":{"base_url":"https://a","config_index":"1"}}]`, ""},
		{"wrong provider", "token-a", `[{"ID":"account-a","Provider":"codex","Attributes":{"base_url":"https://a","config_index":"0"}}]`, ""},
		{"no candidates", "token-a", `[]`, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + test.token))
			request := `{"Options":{"Headers":{"Authorization":["Bearer token-b"]},"Metadata":{"caller_scope":"` + hex.EncodeToString(sum[:]) + `"}},"Candidates":` + test.candidates + `}`
			data, err := s.pick([]byte(request))
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				Handled bool
				AuthID  string
				Reject  bool
			}
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatal(err)
			}
			if !response.Handled || response.AuthID != test.want || response.Reject != (test.want == "") {
				t.Fatalf("response: %s", data)
			}
		})
	}
	// Without the host's authenticated identity, a raw token header is insufficient.
	data, err := s.pick([]byte(`{"Options":{"Headers":{"Authorization":["Bearer token-a"]}},"Candidates":[{"ID":"account-a","Provider":"claude"}]}`))
	if err != nil || !strings.Contains(string(data), `"Reject":true`) {
		t.Fatalf("missing identity: %s, %v", data, err)
	}
}

func TestBindingReplacement(t *testing.T) {
	var s scheduler
	request := []byte(`{"Options":{"Metadata":{}},"Candidates":[]}`)
	for _, raw := range []string{
		`{`,
		"bindings:\n  token-a: [invalid]\n",
		"bindings:\n  token-a: {claude: a}\n  token-a: {claude: b}\n",
		"bindings:\n  ' token-a': {claude: a}\n",
		"bindings:\n  token-a: {claude: ''}\n",
	} {
		valid, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("bindings:\n  token-a: {claude: account-a}\n")})
		if err := s.configure(valid); err != nil {
			t.Fatal(err)
		}
		invalid, _ := json.Marshal(map[string][]byte{"config_yaml": []byte(raw)})
		if err := s.configure(invalid); err == nil || strings.Contains(err.Error(), "token-a") {
			t.Fatalf("expected a configuration error without credential content: %v", err)
		}
		if s.bindings.Load() != nil {
			t.Fatal("invalid replacement kept old authorization")
		}
		data, err := s.pick(request)
		if err != nil || !strings.Contains(string(data), `"Reject":true`) {
			t.Fatalf("invalid configuration did not reject: %s, %v", data, err)
		}
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				if _, err := s.pick(request); err != nil {
					t.Error(err)
				}
			}
		})
	}
	for range 100 {
		if err := s.configure([]byte(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestPluginProtocol(t *testing.T) {
	var s scheduler
	for _, method := range []string{"plugin.register", "plugin.reconfigure"} {
		data, err := s.call(method, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), `"scheduler_across_priorities":true`) {
			t.Fatalf("bound accounts must remain selectable across priorities: %s", data)
		}
	}
	t.Run("MCP policy is injected by default", func(t *testing.T) {
		config, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("bindings:\n  token-a: {claude: a, codex: a}\n")})
		if err := s.configure(config); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00token-a"))
		scope := hex.EncodeToString(sum[:])
		for _, format := range []string{"claude", "openai-response"} {
			for _, stream := range []bool{false, true} {
				for _, system := range []any{"original instructions", []any{map[string]any{"type": "text", "text": "original instructions", "cache_control": map[string]string{"type": "ephemeral"}}}} {
					if format == "openai-response" {
						if _, ok := system.(string); !ok {
							continue
						}
					}
					field := "instructions"
					if format == "claude" {
						field = "system"
					}
					body := map[string]any{"model": "fixture", "stream": stream, field: system, "tools": []any{map[string]any{"name": "mcp__successbox__request_mcp_install", "type": "function", "parameters": map[string]string{"type": "object"}}}, "input": []any{map[string]any{"role": "user", "content": "hello"}}}
					encoded, _ := json.Marshal(body)
					raw, _ := json.Marshal(map[string]any{"SourceFormat": format, "Body": encoded, "Metadata": map[string]string{"caller_scope": scope}})
					result, err := s.call("request.intercept_before", raw)
					if err != nil {
						t.Fatal(err)
					}
					var response struct{ Result struct{ Body []byte } }
					if err := json.Unmarshal(result, &response); err != nil {
						t.Fatal(err)
					}
					var patched map[string]json.RawMessage
					if err := json.Unmarshal(response.Result.Body, &patched); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(string(patched[field]), "$PRIVACY_PROTECT_api_key") || !strings.Contains(string(patched[field]), "original instructions") {
						t.Fatal("instructions not preserved and extended")
					}
					var original map[string]json.RawMessage
					json.Unmarshal(encoded, &original)
					for _, key := range []string{"tools", "input", "model", "stream"} {
						if string(original[key]) != string(patched[key]) {
							t.Fatalf("changed %s", key)
						}
					}
					raw, _ = json.Marshal(map[string]any{"SourceFormat": format, "Body": response.Result.Body, "Metadata": map[string]string{"caller_scope": scope}})
					result, err = s.call("request.intercept_before", raw)
					if err != nil {
						t.Fatal(err)
					}
					response.Result.Body = nil
					json.Unmarshal(result, &response)
					if len(response.Result.Body) != 0 {
						t.Fatal("instructions appended twice")
					}
					delete(body, "tools")
					encoded, _ = json.Marshal(body)
					raw, _ = json.Marshal(map[string]any{"SourceFormat": format, "Body": encoded, "Metadata": map[string]string{"caller_scope": scope}})
					result, err = s.call("request.intercept_before", raw)
					if err != nil {
						t.Fatal(err)
					}
					response.Result.Body = nil
					json.Unmarshal(result, &response)
					if !strings.Contains(string(response.Result.Body), "$PRIVACY_PROTECT_api_key") || !strings.Contains(string(response.Result.Body), "request_mcp_install") {
						t.Fatal("request without tools skipped the default policy")
					}
				}
			}
		}
	})

	t.Run("Responses Lite declares tools inside input", func(t *testing.T) {
		configuration, _ := json.Marshal(map[string][]byte{"config_yaml": []byte("bindings:\n  lite-client: {codex: authorized.json}\n")})
		if err := s.configure(configuration); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00lite-client"))
		// Codex 0.163.0 build_responses_request emits this shape for
		// use_responses_lite models; there is no top-level tools property.
		body := []byte(`{"model":"fixture","stream":true,"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"mcp__successbox","tools":[{"type":"function","name":"request_mcp_install","parameters":{"type":"object"}}]}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"Install project tools"}]}]}`)
		raw, _ := json.Marshal(map[string]any{"SourceFormat": "openai-response", "Body": body, "Metadata": map[string]string{"caller_scope": hex.EncodeToString(sum[:])}})
		result, err := s.call("request.intercept_before", raw)
		if err != nil {
			t.Fatal(err)
		}
		var response struct{ Result struct{ Body []byte } }
		if err := json.Unmarshal(result, &response); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(response.Result.Body), "$PRIVACY_PROTECT_api_key") {
			t.Fatal("Responses Lite request with a registered installation tool skipped the privacy policy")
		}
		var before, after map[string]json.RawMessage
		json.Unmarshal(body, &before)
		json.Unmarshal(response.Result.Body, &after)
		var original, patched []json.RawMessage
		json.Unmarshal(before["input"], &original)
		json.Unmarshal(after["input"], &patched)
		if len(patched) != len(original)+1 || string(patched[0]) != string(original[0]) || string(patched[2]) != string(original[1]) {
			t.Fatal("Lite injection changed the executable tools or original messages")
		}
		var policy struct {
			Type, Role string
			Content    []struct{ Type, Text string }
		}
		json.Unmarshal(patched[1], &policy)
		if policy.Type != "message" || policy.Role != "developer" || len(policy.Content) != 1 || policy.Content[0].Type != "input_text" || !strings.Contains(policy.Content[0].Text, "$PRIVACY_PROTECT_api_key") {
			t.Fatal("Lite policy is not a developer instruction message")
		}
		if _, exists := after["tools"]; exists {
			t.Fatal("Lite injection created a second tool registry")
		}
		if _, exists := after["instructions"]; exists {
			t.Fatal("Lite injection used top-level instructions instead of a developer message")
		}
		// Retrying is a no-op, including when the caller changes its tools.
		// Tool discovery and execution remain owned by the client.
		raw, _ = json.Marshal(map[string]any{"SourceFormat": "openai-response", "Body": response.Result.Body, "Metadata": map[string]string{"caller_scope": hex.EncodeToString(sum[:])}})
		result, err = s.call("request.intercept_before", raw)
		if err != nil {
			t.Fatal(err)
		}
		response.Result.Body = nil
		json.Unmarshal(result, &response)
		if len(response.Result.Body) != 0 {
			t.Fatal("Lite retry duplicated the policy")
		}
		patched[0] = json.RawMessage(`{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"mcp__successbox","tools":[{"type":"function","name":"request_mcp_install"},{"type":"function","name":"project-search"}]}]}`)
		after["input"], _ = json.Marshal(patched)
		updated, _ := json.Marshal(after)
		raw, _ = json.Marshal(map[string]any{"SourceFormat": "openai-response", "Body": updated, "Metadata": map[string]string{"caller_scope": hex.EncodeToString(sum[:])}})
		result, err = s.call("request.intercept_before", raw)
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(result, &response)
		if len(response.Result.Body) != 0 {
			t.Fatal("changing tools changed or duplicated the default policy")
		}
		for _, test := range []struct {
			name, format, caller, body string
			wantPolicy                 bool
		}{
			{"unbound caller", "openai-response", "unknown", string(body), false},
			{"user messages do not control injection", "openai-response", hex.EncodeToString(sum[:]), strings.Replace(string(body), `"role":"developer"`, `"role":"user"`, 1), true},
			{"Claude injects without tools", "claude", hex.EncodeToString(sum[:]), `{"messages":[{"role":"user","content":"hello"}]}`, true},
			{"Code Mode has no direct MCP tools", "openai-response", hex.EncodeToString(sum[:]), `{"input":[{"type":"additional_tools","role":"developer","tools":[{"type":"namespace","name":"functions","tools":[{"type":"custom","name":"exec"}]}]}]}`, true},
		} {
			t.Run(test.name, func(t *testing.T) {
				raw, _ := json.Marshal(map[string]any{"SourceFormat": test.format, "Body": []byte(test.body), "Metadata": map[string]string{"caller_scope": test.caller}})
				result, err := s.call("request.intercept_before", raw)
				if err != nil {
					t.Fatal(err)
				}
				var response struct{ Result struct{ Body []byte } }
				json.Unmarshal(result, &response)
				if strings.Contains(string(response.Result.Body), "$PRIVACY_PROTECT_api_key") != test.wantPolicy {
					t.Fatal("default policy did not follow the authenticated caller binding")
				}
			})
		}
	})

	if _, err := s.call("scheduler.pick", []byte(`{`)); err == nil {
		t.Fatal("malformed request succeeded")
	}
	if _, err := s.call("unknown", nil); err == nil {
		t.Fatal("unknown method succeeded")
	}
}
