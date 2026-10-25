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
	if _, err := s.call("scheduler.pick", []byte(`{`)); err == nil {
		t.Fatal("malformed request succeeded")
	}
	if _, err := s.call("unknown", nil); err == nil {
		t.Fatal("unknown method succeeded")
	}
}
