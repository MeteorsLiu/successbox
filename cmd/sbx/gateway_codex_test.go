package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This opt-in integration helper uses a temporary Codex configuration and a
// host-side snapshot of the explicitly supplied account. The agent receives
// only a gateway key; the user's configuration/authentication store is unchanged.
func gatewayCodexTestSession(t *testing.T, binary, endpoint, authFile string) (func(string, string) bool, func()) {
	t.Helper()
	dir := t.TempDir()
	auth, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatal("read authorized Codex authentication file:", err)
	}
	llmEndpoint := gatewayCodexLLMTestBackend(t, auth)
	model := os.Getenv("SUCCESSBOX_TEST_CODEX_MODEL")
	if model == "" {
		t.Fatal("SUCCESSBOX_TEST_CODEX_MODEL is required for the live LLM gateway test")
	}
	provider := fmt.Sprintf("model = %q\nmodel_provider = \"successbox\"\n[model_providers.successbox]\nname = \"Successbox\"\nbase_url = %q\nenv_key = \"SUCCESSBOX_TEST_LLM_TOKEN\"\nwire_api = \"responses\"\nrequires_openai_auth = false\n\n", model, llmEndpoint)
	// Approve only the fixture's install and search tools; the thread keeps its read-only
	// sandbox and never-ask policy for everything else.
	config := provider + fmt.Sprintf(`[mcp_servers.successbox]
url = %q
bearer_token_env_var = "SUCCESSBOX_TEST_MCP_TOKEN"

[mcp_servers.successbox.tools.project-search]
approval_mode = "approve"

[mcp_servers.successbox.tools.request_mcp_install]
approval_mode = "approve"
`, endpoint+"/mcp")
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "app-server", "--stdio")
	command.Dir = dir
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CODEX_HOME=") && !strings.HasPrefix(entry, "SUCCESSBOX_TEST_MCP_TOKEN=") && !strings.HasPrefix(entry, "SUCCESSBOX_TEST_LLM_TOKEN=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "CODEX_HOME="+dir, "SUCCESSBOX_TEST_MCP_TOKEN=sk-bf-fixture-A", "SUCCESSBOX_TEST_LLM_TOKEN=successbox-live-client")
	log, err := os.OpenFile(filepath.Join(dir, "codex-stderr.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = log
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	messages := make(chan map[string]json.RawMessage, 128)
	go func() {
		defer close(messages)
		scan := bufio.NewScanner(output)
		scan.Buffer(make([]byte, 65536), 16<<20)
		for scan.Scan() {
			var message map[string]json.RawMessage
			if json.Unmarshal(scan.Bytes(), &message) == nil {
				messages <- message
			}
		}
	}()
	t.Cleanup(func() {
		// The fixture and its helpers own the SSE connection. Stop them before
		// the earlier-registered HTTP server cleanup waits for connections.
		_ = input.Close()
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		for range messages {
		}
		_ = command.Wait()
		_ = log.Close()
	})
	next := func() map[string]json.RawMessage {
		t.Helper()
		select {
		case message, ok := <-messages:
			if !ok {
				t.Fatal("Codex app-server exited")
			}
			return message
		case <-time.After(120 * time.Second):
			t.Fatal("Codex app-server did not finish the requested operation within 120 seconds")
			return nil
		}
	}
	seq := 0
	send := func(method string, params any) int {
		t.Helper()
		seq++
		raw, err := json.Marshal(map[string]any{"id": seq, "method": method, "params": params})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fmt.Fprintln(input, string(raw)); err != nil {
			t.Fatal(err)
		}
		return seq
	}
	response := func(id int) json.RawMessage {
		t.Helper()
		for {
			msg := next()
			if string(msg["id"]) != fmt.Sprint(id) {
				continue
			}
			if len(msg["error"]) > 0 {
				t.Fatalf("Codex request %d returned a protocol error (details withheld from test output)", id)
			}
			return msg["result"]
		}
	}
	response(send("initialize", map[string]any{"clientInfo": map[string]string{"name": "successbox-integration", "version": "1"}, "capabilities": map[string]any{"experimentalApi": true}}))
	if _, err := io.WriteString(input, "{\"method\":\"initialized\"}\n"); err != nil {
		t.Fatal(err)
	}
	raw := response(send("thread/start", map[string]any{"cwd": dir, "approvalPolicy": "never", "sandbox": "read-only"}))
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(raw, &started); err != nil || started.Thread.ID == "" {
		t.Fatal("Codex did not create a test thread")
	}
	// Discovery uses this loaded thread, ensuring the original catalog exists
	// before the fixture installs the additional MCP.
	raw = response(send("mcpServerStatus/list", map[string]any{"threadId": started.Thread.ID, "serverName": "successbox"}))
	if !strings.Contains(string(raw), "request_mcp_install") || strings.Contains(string(raw), "project-search") {
		t.Fatal("Codex did not load the expected initial MCP tool catalog")
	}
	t.Log("Codex thread created with only the installation tool")
	turn := func(prompt, expectedTool string) bool {
		t.Helper()
		id := send("turn/start", map[string]any{"threadId": started.Thread.ID, "input": []any{map[string]string{"type": "text", "text": prompt}}})
		called := false
		for {
			msg := next()
			if string(msg["id"]) == fmt.Sprint(id) && len(msg["error"]) > 0 {
				t.Fatal("Codex turn/start failed")
			}
			var method string
			_ = json.Unmarshal(msg["method"], &method)
			if method == "item/completed" {
				var event struct {
					Item struct {
						Type   string          `json:"type"`
						Tool   string          `json:"tool"`
						Result json.RawMessage `json:"result"`
						Text   string          `json:"text"`
						Status string          `json:"status"`
					} `json:"item"`
				}
				_ = json.Unmarshal(msg["params"], &event)
				t.Logf("Codex completed item: type=%s tool=%s status=%s", event.Item.Type, event.Item.Tool, event.Item.Status)
				if event.Item.Type == "agentMessage" {
					t.Logf("Codex fixture reply: %.600s", event.Item.Text)
				}
				if event.Item.Type == "mcpToolCall" && event.Item.Tool == expectedTool && event.Item.Status == "completed" {
					result := string(event.Item.Result)
					called = (expectedTool == "project-search" && strings.Contains(result, "successbox-hot-update-ok")) || (expectedTool == "request_mcp_install" && strings.Contains(result, "pending"))
				}
			}
			if method == "turn/completed" {
				var event struct {
					Turn struct {
						Status string `json:"status"`
					} `json:"turn"`
				}
				_ = json.Unmarshal(msg["params"], &event)
				if event.Turn.Status != "completed" {
					t.Fatal("Codex model turn did not complete successfully")
				}
				return called
			}
		}
	}
	reload := func() {
		t.Helper()
		if os.Getenv("SUCCESSBOX_TEST_CODEX_RECONNECT") == "1" {
			// This opt-in experiment changes only the fixture's private config.
			// Inspecting the selected server waits for the disabled publication
			// before re-enabling it, so refresh cannot reuse the previous client.
			response(send("config/value/write", map[string]any{"keyPath": "mcp_servers.successbox.enabled", "mergeStrategy": "replace", "value": false, "filePath": filepath.Join(dir, "config.toml")}))
			response(send("config/mcpServer/reload", nil))
			disabled := response(send("mcpServerStatus/list", map[string]any{"threadId": started.Thread.ID, "serverName": "successbox"}))
			if strings.Contains(string(disabled), "request_mcp_install") || strings.Contains(string(disabled), "project-search") {
				t.Fatal("Codex's existing thread retained executable tools after disabling the MCP")
			}
			response(send("config/value/write", map[string]any{"keyPath": "mcp_servers.successbox.enabled", "mergeStrategy": "replace", "value": true, "filePath": filepath.Join(dir, "config.toml")}))
			t.Log("Disabled and re-enabled only the fixture MCP using existing configuration APIs")
		}
		response(send("config/mcpServer/reload", nil))
		raw := response(send("mcpServerStatus/list", map[string]any{"threadId": started.Thread.ID, "serverName": "successbox"}))
		if !strings.Contains(string(raw), "project-search") {
			t.Fatal("Codex's existing thread still lacks the new MCP tool after config/mcpServer/reload")
		}
		t.Log("Codex's existing thread discovered project-search after config/mcpServer/reload")
	}
	return turn, reload
}
