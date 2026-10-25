package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func gatewayClaudeTestSession(t *testing.T, binary, endpoint, authFile string) func(string, string) bool {
	t.Helper()
	dir := t.TempDir()
	auth, err := os.ReadFile(authFile)
	if err != nil {
		t.Fatal("read authorized proxy authentication file:", err)
	}
	llm := gatewayCodexLLMTestBackend(t, auth)
	model := os.Getenv("SUCCESSBOX_TEST_CODEX_MODEL")
	if model == "" {
		t.Fatal("SUCCESSBOX_TEST_CODEX_MODEL is required for the Claude gateway test")
	}
	config, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"successbox": map[string]any{"type": "http", "url": endpoint + "/mcp", "headers": map[string]string{"Authorization": "Bearer sk-bf-fixture-A"}}}})
	configPath := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(configPath, config, 0600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "--bare", "--print", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--no-session-persistence", "--strict-mcp-config", "--mcp-config", configPath, "--model", model, "--tools", "", "--allowedTools", "mcp__successbox__request_mcp_install,mcp__successbox__project-search", "--permission-mode", "dontAsk")
	command.Dir = dir
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "ANTHROPIC_") && !strings.HasPrefix(entry, "CLAUDE_CONFIG_DIR=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, "CLAUDE_CONFIG_DIR="+dir, "ANTHROPIC_BASE_URL="+strings.TrimSuffix(llm, "/v1"), "ANTHROPIC_API_KEY=successbox-live-client")
	log, err := os.OpenFile(filepath.Join(dir, "claude-stderr.log"), os.O_CREATE|os.O_WRONLY, 0600)
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
		log.Close()
		t.Fatal(err)
	}
	messages := make(chan json.RawMessage, 128)
	go func() {
		defer close(messages)
		scan := bufio.NewScanner(output)
		scan.Buffer(make([]byte, 65536), 16<<20)
		for scan.Scan() {
			messages <- append(json.RawMessage(nil), scan.Bytes()...)
		}
	}()
	t.Cleanup(func() {
		_ = input.Close()
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		for range messages {
		}
		_ = command.Wait()
		_ = log.Close()
	})
	session := ""
	return func(prompt, expectedTool string) bool {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]string{"role": "user", "content": prompt}})
		if _, err := fmt.Fprintln(input, string(raw)); err != nil {
			t.Fatal(err)
		}
		deadline := time.NewTimer(120 * time.Second)
		defer deadline.Stop()
		toolID, called := "", false
		for {
			var raw json.RawMessage
			select {
			case message, ok := <-messages:
				if !ok {
					t.Fatal("Claude Code exited before completing the requested turn")
				}
				raw = message
			case <-deadline.C:
				t.Fatal("Claude Code did not complete the requested turn within 120 seconds")
			}
			var event struct {
				Type      string
				Subtype   string
				SessionID string `json:"session_id"`
				IsError   bool   `json:"is_error"`
				Message   struct {
					Content []struct {
						Type, Name, ID string
						ToolUseID      string          `json:"tool_use_id"`
						Content        json.RawMessage `json:"content"`
						IsError        bool            `json:"is_error"`
					}
				}
			}
			if json.Unmarshal(raw, &event) != nil {
				t.Fatal("invalid Claude stream message")
			}
			if event.SessionID != "" {
				if session != "" && session != event.SessionID {
					t.Fatal("Claude switched conversations during installation")
				}
				session = event.SessionID
			}
			for _, content := range event.Message.Content {
				if event.Type == "assistant" && content.Type == "tool_use" && content.Name == "mcp__successbox__"+expectedTool {
					toolID = content.ID
					t.Logf("Claude called fixture tool %s", expectedTool)
				}
				if event.Type == "user" && content.Type == "tool_result" && toolID != "" && content.ToolUseID == toolID && !content.IsError {
					result := string(content.Content)
					called = (expectedTool == "project-search" && strings.Contains(result, "successbox-hot-update-ok")) || (expectedTool == "request_mcp_install" && strings.Contains(result, "pending"))
				}
			}
			if event.Type == "result" {
				if event.IsError {
					t.Fatalf("Claude turn failed (%s); response contents withheld", event.Subtype)
				}
				return called
			}
		}
	}
}
