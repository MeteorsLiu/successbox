package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	gatewaypb "github.com/MeteorsLiu/successbox/api/gateway/v1"
)

func gatewayMCPTestBackend(t *testing.T, upstream string) string {
	t.Helper()
	if binary := os.Getenv("SUCCESSBOX_TEST_BIFROST"); binary != "" {
		dir := t.TempDir()
		pricing := filepath.Join(dir, "pricing.json")
		parameters := filepath.Join(dir, "parameters.json")
		for path, content := range map[string]string{pricing: `{"fixture":{"mode":"chat","litellm_provider":"openai","input_cost_per_token":0,"output_cost_per_token":0}}`, parameters: `{}`} {
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
		}
		config := map[string]any{"disable_onboarding": true, "client": map[string]any{"enable_logging": false, "disable_content_logging": true, "enforce_governance_header": true, "mcp_disable_auto_tool_inject": true}, "mcp": map[string]any{"client_configs": []any{}}, "governance": map[string]any{"auth_config": map[string]any{"is_enabled": true, "admin_username": "fixture", "admin_password": "fixture-admin"}, "virtual_keys": []any{map[string]any{"id": "A", "name": "A", "value": "sk-bf-fixture-A", "is_active": true}, map[string]any{"id": "B", "name": "B", "value": "sk-bf-fixture-B", "is_active": true}}}, "framework": map[string]any{"pricing": map[string]any{"pricing_url": "file://" + pricing, "model_parameters_url": "file://" + parameters, "mcp_library_sync_interval": 0, "live_models_sync_interval": 0}}}
		raw, _ := json.Marshal(config)
		if err := os.WriteFile(filepath.Join(dir, "config.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()
		log, err := os.Create(filepath.Join(dir, "bifrost.log"))
		if err != nil {
			t.Fatal(err)
		}
		command := exec.Command(binary, "-host", "127.0.0.1", "-port", fmt.Sprint(port), "-app-dir", dir, "-log-level", "warn")
		command.Stdout = log
		command.Stderr = log
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- command.Wait() }()
		t.Cleanup(func() {
			_ = command.Process.Signal(syscall.SIGTERM)
			select {
			case <-exited:
			case <-time.After(15 * time.Second):
				t.Error("Bifrost did not stop after SIGTERM")
			}
			log.Close()
		})
		deadline := time.Now().Add(30 * time.Second)
		for {
			conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
			if err == nil {
				conn.Close()
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("Bifrost startup deadline exceeded")
			}
			time.Sleep(50 * time.Millisecond)
		}
		return fmt.Sprintf("http://127.0.0.1:%d", port)
	}
	var mu sync.Mutex
	registered := false
	grants := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			user, pass, ok := r.BasicAuth()
			if !ok || user != "fixture" || pass != "fixture-admin" {
				http.Error(w, "unauthorized", 401)
				return
			}
		}
		write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
		switch r.URL.Path {
		case "/api/mcp/clients":
			clients := []any{}
			if registered {
				clients = append(clients, map[string]any{"config": map[string]any{"client_id": "native-client-id", "name": "project", "connection_type": "http", "connection_string": map[string]string{"value": upstream}}, "tools": []any{map[string]any{"name": "search"}}, "state": "connected"})
			}
			write(map[string]any{"clients": clients, "total_count": len(clients)})
		case "/api/mcp/client":
			var v map[string]any
			if json.NewDecoder(r.Body).Decode(&v) != nil {
				http.Error(w, "invalid", 400)
				return
			}
			if v["allow_by_default"] != false {
				t.Error("client was not deny-by-default")
			}
			registered = true
			write(map[string]string{"status": "success"})
		case "/api/governance/virtual-keys/A", "/api/governance/virtual-keys/B":
			id := strings.TrimPrefix(r.URL.Path, "/api/governance/virtual-keys/")
			if r.Method == http.MethodPut {
				grants[id] = true
				write(map[string]any{})
				return
			}
			configs := []any{}
			if grants[id] {
				configs = append(configs, map[string]any{"id": 1, "mcp_client_name": "project", "tools_to_execute": []string{"search"}})
			}
			write(map[string]any{"virtual_key": map[string]any{"mcp_configs": configs}})
		case "/mcp":
			id := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer sk-bf-fixture-")
			if id != "A" && id != "B" {
				http.Error(w, "unauthorized", 401)
				return
			}
			var msg struct {
				ID     any             `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.NewDecoder(r.Body).Decode(&msg) != nil {
				http.Error(w, "invalid", 400)
				return
			}
			var result any
			switch msg.Method {
			case "initialize":
				result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}
			case "tools/list":
				tools := []any{}
				if grants[id] {
					tools = append(tools, map[string]any{"name": "project-search", "description": "fixture search", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"api_key": map[string]string{"type": "string"}}}})
				}
				result = map[string]any{"tools": tools}
			case "tools/call":
				var params struct {
					Arguments map[string]any `json:"arguments"`
				}
				json.Unmarshal(msg.Params, &params)
				if params.Arguments["api_key"] != "fixture-secret-install" {
					t.Error("placeholder was not resolved")
				}
				result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "successbox-hot-update-ok"}}}
			default:
				w.WriteHeader(202)
				return
			}
			write(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestGatewaySessionMCPInstallApproval(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(405)
			return
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		if json.NewDecoder(r.Body).Decode(&msg) != nil {
			w.WriteHeader(400)
			return
		}
		if len(msg.ID) == 0 {
			w.WriteHeader(202)
			return
		}
		var result any
		switch msg.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "project", "version": "1"}}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "search", "description": "fixture search", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"api_key": map[string]string{"type": "string"}}, "required": []string{"api_key"}}}}}
		case "tools/call":
			upstreamCalls.Add(1)
			if msg.Params.Arguments["api_key"] != "fixture-secret-install" {
				t.Error("upstream did not receive resolved field")
			}
			result = map[string]any{"content": []any{map[string]string{"type": "text", "text": "successbox-hot-update-ok"}}, "isError": false}
		default:
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
	}))
	t.Cleanup(upstream.Close)
	backend := gatewayMCPTestBackend(t, upstream.URL)
	server, _ := gatewayTestServer(t)
	adapter := &gatewayMCP{base: backend, username: "fixture", password: "fixture-admin", client: &http.Client{Timeout: 10 * time.Second}, control: server, keys: map[string]string{"A": "sk-bf-fixture-A", "B": "sk-bf-fixture-B"}, sessions: make(map[string]*gatewayMCPSession)}
	server.mcp = adapter
	endpoint := httptest.NewServer(http.HandlerFunc(adapter.serveHTTP))
	t.Cleanup(endpoint.Close)
	var agentTurn func(string, string) bool
	var refreshTools func()
	if binary := os.Getenv("SUCCESSBOX_TEST_CODEX"); binary != "" {
		authFile := os.Getenv("SUCCESSBOX_TEST_CODEX_AUTH")
		if authFile == "" {
			t.Fatal("SUCCESSBOX_TEST_CODEX_AUTH must point to an authorized Codex auth file")
		}
		agentTurn, refreshTools = gatewayCodexTestSession(t, binary, endpoint.URL, authFile)
	} else if binary := os.Getenv("SUCCESSBOX_TEST_CLAUDE"); binary != "" {
		agentTurn = gatewayClaudeTestSession(t, binary, endpoint.URL, os.Getenv("SUCCESSBOX_TEST_CODEX_AUTH"))
		refreshTools = func() {} // Claude must consume the native MCP listChanged notification.
	}
	if agentTurn != nil {
		agentTurn("This is a sandbox gateway integration test. Reply READY only, without calling any tools.", "")
		t.Log("Agent completed the initial model turn before MCP installation")
	}
	client := &http.Client{Timeout: 15 * time.Second}
	call := func(token, session, method string, params any) (map[string]any, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "call", "method": method, "params": params})
		req, _ := http.NewRequest(http.MethodPost, endpoint.URL+"/mcp", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set("Mcp-Session-Id", session)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			data, _ := io.ReadAll(resp.Body)
			t.Fatalf("MCP %s: HTTP %d %s", method, resp.StatusCode, data)
		}
		var result map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
		return result, resp.Header.Get("Mcp-Session-Id")
	}
	init := map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "successbox-test", "version": "1"}}
	_, sessionA := call(adapter.keys["A"], "", "initialize", init)
	_, sessionB := call(adapter.keys["B"], "", "initialize", init)
	initial, _ := call(adapter.keys["A"], sessionA, "tools/list", map[string]any{})
	raw, _ := json.Marshal(initial)
	if !bytes.Contains(raw, []byte("request_mcp_install")) || bytes.Contains(raw, []byte("project-search")) {
		t.Fatalf("initial tools: %s", raw)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+adapter.keys["A"])
	req.Header.Set("Mcp-Session-Id", sessionA)
	// The notification stream stays open while live model turns run; the
	// ordinary call client's 15-second timeout must not end this SSE session.
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Body.Close()
	notified := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stream.Body)
		for scanner.Scan() {
			if strings.Contains(scanner.Text(), "notifications/tools/list_changed") {
				notified <- scanner.Text()
				return
			}
		}
		notified <- ""
	}()
	front := gatewayTestClient(t, server, "frontend", "front")
	openGatewayTestSession(t, front, "A")
	params := map[string]any{"name": "request_mcp_install", "arguments": map[string]any{"mcp_name": "project", "transport": "http", "endpoint": upstream.URL, "requested_tools": []string{"search"}, "credential_fields": []string{"api_key"}}}
	if agentTurn != nil {
		arguments, _ := json.Marshal(params["arguments"])
		if !agentTurn("Call the registered request_mcp_install tool once with exactly these arguments: "+string(arguments)+". This requests approval from the separate frontend. Do not read files, run shell commands, request real credentials, or call another tool. Stop when the request is pending.", "request_mcp_install") {
			t.Fatal("Agent did not submit an installation request through the MCP gateway")
		}
	}
	submitted, _ := call(adapter.keys["A"], sessionA, "tools/call", params)
	if submitted["error"] != nil {
		t.Fatalf("submit: %v", submitted)
	}
	approval, err := front.Recv()
	if err != nil {
		t.Fatal(err)
	}
	install := approval.GetRequest().GetMcpInstallApproval()
	if install == nil {
		t.Fatalf("approval: %v", approval)
	}
	duplicate, _ := call(adapter.keys["A"], sessionA, "tools/call", params)
	if fmt.Sprint(submitted) != fmt.Sprint(duplicate) {
		t.Fatal("identical install was not deduplicated")
	}
	if err := front.Send(&gatewaypb.GatewayClientMessage{CorrelationId: "approve", Body: &gatewaypb.GatewayClientMessage_Response{Response: &gatewaypb.ClientResponse{Body: &gatewaypb.ClientResponse_DecideMcpInstall{DecideMcpInstall: &gatewaypb.DecideMCPInstall{OperationId: "approve", RequestId: install.RequestId, Approve: true}}}}}); err != nil {
		t.Fatal(err)
	}
	var mcpID string
	for {
		msg, err := front.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if failure := msg.GetResponse().GetOperationError(); failure != nil {
			t.Fatalf("installation: %v", failure)
		}
		if event := msg.GetEvent().GetMcpInstallChanged(); event != nil {
			mcpID = event.McpId
		}
		input := msg.GetRequest().GetSecretInput()
		if input == nil {
			continue
		}
		if mcpID == "" || input.McpId != mcpID {
			t.Fatal("frontend did not receive the Bifrost client ID")
		}
		sendGatewayRequest(t, front, "fill", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_CreateMcpSecret{CreateMcpSecret: &gatewaypb.CreateMCPSecret{SandboxId: "A", McpId: mcpID, McpName: "project", FieldName: "api_key", SecretValue: []byte("fixture-secret-install")}}})
		break
	}
	approved := false
	filled := false
	for !approved || !filled {
		msg, err := front.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if failure := msg.GetResponse().GetOperationError(); failure != nil {
			t.Fatalf("installation: %v", failure)
		}
		if msg.GetResponse().GetMutationAccepted() != nil {
			filled = true
		}
		if decision := msg.GetResponse().GetMcpInstallDecisionAccepted(); decision != nil {
			approved = decision.Change == gatewaypb.MCPInstallChange_MCP_INSTALL_APPROVED
		}
	}
	select {
	case n := <-notified:
		if n == "" {
			t.Fatal("SSE ended without notification")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tool-list notification missing")
	}
	tools, _ := call(adapter.keys["A"], sessionA, "tools/list", map[string]any{})
	raw, _ = json.Marshal(tools)
	if !bytes.Contains(raw, []byte("project-search")) {
		t.Fatalf("new tool missing: %s", raw)
	}
	result, _ := call(adapter.keys["A"], sessionA, "tools/call", map[string]any{"name": "project-search", "arguments": map[string]string{"api_key": "$PRIVACY_PROTECT_api_key"}})
	raw, _ = json.Marshal(result)
	if !bytes.Contains(raw, []byte("successbox-hot-update-ok")) || bytes.Contains(raw, []byte("fixture-secret-install")) {
		t.Fatalf("tool call: %s", raw)
	}
	other, _ := call(adapter.keys["B"], sessionB, "tools/list", map[string]any{})
	raw, _ = json.Marshal(other)
	if bytes.Contains(raw, []byte("project-search")) {
		t.Fatal("B can discover A's tool")
	}
	denied, _ := call(adapter.keys["B"], sessionB, "tools/call", map[string]any{"name": "project-search", "arguments": map[string]string{"api_key": "$PRIVACY_PROTECT_api_key"}})
	if denied["error"] == nil {
		t.Fatal("B can call A's tool")
	}
	beforeInvalid := upstreamCalls.Load()
	for _, arguments := range []map[string]string{
		{"api_key": "$PRIVACY_PROTECT_password"},
		{"unregistered": "$PRIVACY_PROTECT_unregistered"},
	} {
		invalid, _ := call(adapter.keys["A"], sessionA, "tools/call", map[string]any{"name": "project-search", "arguments": arguments})
		if invalid["error"] == nil {
			t.Fatal("invalid placeholder was accepted")
		}
	}
	if upstreamCalls.Load() != beforeInvalid {
		t.Fatal("invalid placeholder reached the upstream")
	}
	frontB := gatewayTestClient(t, server, "frontend", "front")
	openGatewayTestSession(t, frontB, "B")
	requestB, _ := call(adapter.keys["B"], sessionB, "tools/call", map[string]any{"name": "request_mcp_install", "arguments": map[string]any{"mcp_name": "rejected", "transport": "http", "endpoint": upstream.URL, "requested_tools": []string{"search"}}})
	if requestB["error"] != nil {
		t.Fatalf("submit B: %v", requestB)
	}
	messageB, err := frontB.Recv()
	if err != nil || messageB.GetRequest().GetMcpInstallApproval() == nil {
		t.Fatal("B did not receive its installation request:", err)
	}
	pendingB := messageB.GetRequest().GetMcpInstallApproval()
	if err := frontB.CloseSend(); err != nil {
		t.Fatal(err)
	}
	frontB = gatewayTestClient(t, server, "frontend", "front")
	openGatewayTestSession(t, frontB, "B")
	sendGatewayRequest(t, frontB, "resume", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_ResumeSession{ResumeSession: &gatewaypb.ResumeSession{ReplayPending: true}}})
	messageB, err = frontB.Recv()
	if err != nil || messageB.GetRequest().GetMcpInstallApproval().GetRequestId() != pendingB.RequestId {
		t.Fatal("frontend reconnect did not replay the pending installation:", err)
	}
	if err := frontB.Send(&gatewaypb.GatewayClientMessage{CorrelationId: "reject", Body: &gatewaypb.GatewayClientMessage_Response{Response: &gatewaypb.ClientResponse{Body: &gatewaypb.ClientResponse_DecideMcpInstall{DecideMcpInstall: &gatewaypb.DecideMCPInstall{OperationId: "reject", RequestId: pendingB.RequestId, Approve: false}}}}}); err != nil {
		t.Fatal(err)
	}
	if gatewayTestResponse(t, frontB, "reject").GetMcpInstallDecisionAccepted().GetChange() != gatewaypb.MCPInstallChange_MCP_INSTALL_REJECTED {
		t.Fatal("installation rejection was not acknowledged")
	}
	clients, err := adapter.clients(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range clients {
		if client.Config.Name == "rejected" {
			t.Fatal("rejected installation was registered with Bifrost")
		}
	}
	reused, err := adapter.register(context.Background(), install)
	if err != nil || reused != mcpID {
		t.Fatalf("registered endpoint was not reused: id=%q error=%v", reused, err)
	}
	t.Log("same-session install, real client ID, secret input, SSE notification, tool execution, endpoint reuse and A/B isolation passed")
	if agentTurn != nil {
		refreshTools()
		callsBefore := upstreamCalls.Load()
		// The gateway policy is the only source of the placeholder syntax in
		// the model conversation. This also detects skipped prompt injection.
		if !agentTurn("The project-search MCP tool is now installed and approved. Call it once to verify the connection, following the gateway's instructions for sensitive fields. Do not request a real key, use shell tools, or read files. Report its result.", "project-search") {
			t.Fatalf("Agent did not successfully call the newly installed tool in the same thread; observed upstream calls during model turn: %d", upstreamCalls.Load()-callsBefore)
		}
		if os.Getenv("SUCCESSBOX_TEST_BIFROST") != "" && upstreamCalls.Load()-callsBefore != 1 {
			t.Fatalf("expected exactly one model-driven upstream call, got %d", upstreamCalls.Load()-callsBefore)
		}
		t.Log("Agent called the new MCP tool in the same conversation")
	}

}
