package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"

	gatewaypb "github.com/MeteorsLiu/successbox/api/gateway/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type gatewayMCP struct {
	base               string
	username, password string
	client             *http.Client
	control            *gatewayServer
	// Keys are Bifrost virtual-key IDs, the host's existing sandbox binding.
	// Values are host-side virtual keys; neither is taken from tool arguments.
	keys     map[string]string
	mu       sync.Mutex
	sessions map[string]*gatewayMCPSession
	// Serialize registry and virtual-key read/modify/write operations, never
	// user input or tool execution.
	registryMu sync.Mutex
}

type gatewayMCPSession struct {
	sandbox string
	changed chan struct{}
	closed  chan struct{}
}

type gatewayMCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type gatewayMCPClient struct {
	Config struct {
		ID             string `json:"client_id"`
		Name           string `json:"name"`
		ConnectionType string `json:"connection_type"`
		// Bifrost serializes SecretVar as an object even for a literal URL.
		// Only its value is needed to compare an existing endpoint.
		ConnectionString struct {
			Value string `json:"value"`
		} `json:"connection_string"`
		StdioConfig json.RawMessage `json:"stdio_config"`
	} `json:"config"`
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
	State string `json:"state"`
}

// admin only accepts paths constructed by this adapter. It never sends the
// management credential to an endpoint supplied by an agent.
func (g *gatewayMCP) admin(ctx context.Context, method, path string, value, result any) error {
	var body io.Reader
	if value != nil {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.base+path, body)
	if err != nil {
		return err
	}
	req.SetBasicAuth(g.username, g.password)
	req.Header.Set("Content-Type", "application/json")
	resp, err := g.client.Do(req)
	if err != nil {
		return errors.New("Bifrost management request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Bifrost management returned HTTP %d", resp.StatusCode)
	}
	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return errors.New("invalid Bifrost management response")
		}
	}
	return nil
}

func (g *gatewayMCP) clients(ctx context.Context) ([]gatewayMCPClient, error) {
	var clients []gatewayMCPClient
	for offset := 0; ; {
		var page struct {
			Clients []gatewayMCPClient `json:"clients"`
			Total   int                `json:"total_count"`
		}
		if err := g.admin(ctx, http.MethodGet, fmt.Sprintf("/api/mcp/clients?limit=100&offset=%d", offset), nil, &page); err != nil {
			return nil, err
		}
		clients = append(clients, page.Clients...)
		offset += len(page.Clients)
		if offset >= page.Total {
			return clients, nil
		}
		if len(page.Clients) == 0 {
			return nil, errors.New("Bifrost client listing did not advance")
		}
	}
}

func (g *gatewayMCP) register(ctx context.Context, r *gatewaypb.MCPInstallApprovalRequest) (string, error) {
	g.registryMu.Lock()
	defer g.registryMu.Unlock()
	clients, err := g.clients(ctx)
	if err != nil {
		return "", err
	}
	for _, client := range clients {
		if client.Config.Name != r.McpName {
			continue
		}
		same := client.Config.ConnectionType == r.Transport && client.Config.ConnectionString.Value == r.Endpoint
		if r.Transport == "stdio" {
			var a, b any
			same = client.Config.ConnectionType == r.Transport && json.Unmarshal(client.Config.StdioConfig, &a) == nil && json.Unmarshal([]byte(r.Endpoint), &b) == nil
			if same {
				x, _ := json.Marshal(a)
				y, _ := json.Marshal(b)
				same = bytes.Equal(x, y)
			}
		}
		if !same {
			return "", errors.New("an MCP with this name has a different configuration")
		}
		return client.Config.ID, nil
	}
	config := map[string]any{"name": r.McpName, "connection_type": r.Transport, "auth_type": "none", "allow_by_default": false, "tools_to_execute": r.RequestedTools}
	if r.Transport == "stdio" {
		var stdio struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Envs    []string `json:"envs"`
		}
		dec := json.NewDecoder(strings.NewReader(r.Endpoint))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&stdio); err != nil || stdio.Command == "" {
			return "", errors.New("stdio endpoint must be a JSON command/args/envs description")
		}
		config["stdio_config"] = stdio
	} else {
		config["connection_string"] = r.Endpoint
	}
	if err := g.admin(ctx, http.MethodPost, "/api/mcp/client", config, nil); err != nil {
		return "", err
	}
	// Bifrost's creation response has no ID. Read the persisted client, whose
	// client_id was assigned by Bifrost before it connected to the upstream.
	clients, err = g.clients(ctx)
	if err != nil {
		return "", err
	}
	for _, client := range clients {
		if client.Config.Name == r.McpName && client.Config.ID != "" {
			return client.Config.ID, nil
		}
	}
	return "", errors.New("registered MCP client was not returned by Bifrost")
}

func (g *gatewayMCP) grant(ctx context.Context, r *gatewaypb.MCPInstallApprovalRequest) error {
	g.registryMu.Lock()
	defer g.registryMu.Unlock()
	var current struct {
		Key struct {
			Configs []struct {
				ID    uint     `json:"id,omitempty"`
				Name  string   `json:"mcp_client_name"`
				Tools []string `json:"tools_to_execute"`
			} `json:"mcp_configs"`
		} `json:"virtual_key"`
	}
	path := "/api/governance/virtual-keys/" + url.PathEscape(r.SandboxId)
	if err := g.admin(ctx, http.MethodGet, path, nil, &current); err != nil {
		return err
	}
	entries := make([]map[string]any, 0, len(current.Key.Configs)+1)
	found := false
	for _, c := range current.Key.Configs {
		tools := c.Tools
		if c.Name == r.McpName {
			tools = append([]string(nil), tools...)
			for _, tool := range r.RequestedTools {
				if !slices.Contains(tools, tool) {
					tools = append(tools, tool)
				}
			}
			found = true
		}
		entries = append(entries, map[string]any{"id": c.ID, "mcp_client_name": c.Name, "tools_to_execute": tools})
	}
	if !found {
		entries = append(entries, map[string]any{"mcp_client_name": r.McpName, "tools_to_execute": r.RequestedTools})
	}
	return g.admin(ctx, http.MethodPut, path, map[string]any{"mcp_configs": entries}, nil)
}

func (g *gatewayMCP) request(ctx context.Context, sandbox string, raw []byte) (json.RawMessage, int, error) {
	key := g.keys[sandbox]
	if key == "" {
		return nil, http.StatusForbidden, errors.New("sandbox has no MCP binding")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.base+"/mcp", bytes.NewReader(raw))
	if err != nil {
		return nil, 500, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, 502, errors.New("MCP gateway unavailable")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 502, errors.New("read MCP response failed")
	}
	return data, resp.StatusCode, nil
}

func (g *gatewayMCP) tools(ctx context.Context, sandbox string) ([]gatewayMCPTool, error) {
	raw, code, err := g.request(ctx, sandbox, []byte(`{"jsonrpc":"2.0","id":"catalog","method":"tools/list","params":{}}`))
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, errors.New("MCP tool authorization failed")
	}
	var response struct {
		Result struct {
			Tools []gatewayMCPTool `json:"tools"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(raw, &response) != nil || len(response.Error) > 0 {
		return nil, errors.New("MCP tool discovery failed")
	}
	return response.Result.Tools, nil
}

func (g *gatewayMCP) notify(sandbox string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, session := range g.sessions {
		if session.sandbox == sandbox {
			select {
			case session.changed <- struct{}{}:
			default:
			}
		}
	}
}

// serveHTTP is the existing /mcp data endpoint. Management APIs are never
// proxied. The only local tool is request_mcp_install; Bifrost executes others.
func (g *gatewayMCP) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	sandbox := ""
	for id, key := range g.keys {
		if key != "" && key == auth {
			sandbox = id
			break
		}
	}
	if sandbox == "" {
		http.Error(w, "invalid MCP credential", http.StatusUnauthorized)
		return
	}
	sessionID := r.Header.Get("Mcp-Session-Id")
	g.mu.Lock()
	session := g.sessions[sessionID]
	g.mu.Unlock()
	if sessionID != "" && (session == nil || session.sandbox != sandbox) {
		http.Error(w, "MCP session not found", http.StatusNotFound)
		return
	}
	if r.Method == http.MethodGet {
		if session == nil {
			http.Error(w, "MCP session required", 400)
			return
		}
		if _, err := g.tools(r.Context(), sandbox); err != nil {
			http.Error(w, "MCP authorization failed", 403)
			return
		}
		f, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "stream unavailable", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = io.WriteString(w, ": connected\n\n")
		f.Flush()
		for {
			select {
			case <-session.changed:
				if _, err := io.WriteString(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/tools/list_changed\"}\n\n"); err != nil {
					return
				}
				f.Flush()
			case <-session.closed:
				return
			case <-r.Context().Done():
				return
			}
		}
	}
	if r.Method == http.MethodDelete {
		g.mu.Lock()
		if current := g.sessions[sessionID]; current != nil && current == session {
			delete(g.sessions, sessionID)
			close(session.closed)
		}
		g.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id,omitempty"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil || json.Unmarshal(raw, &message) != nil || message.JSONRPC != "2.0" {
		http.Error(w, "invalid MCP request", 400)
		return
	}
	if message.Method != "initialize" && session == nil {
		http.Error(w, "MCP session required", 400)
		return
	}
	reply := func(result any, err error) {
		w.Header().Set("Content-Type", "application/json")
		envelope := map[string]any{"jsonrpc": "2.0", "id": message.ID}
		if err != nil {
			envelope["error"] = map[string]any{"code": -32000, "message": err.Error()}
		} else {
			envelope["result"] = result
		}
		_ = json.NewEncoder(w).Encode(envelope)
	}
	if message.Method == "tools/call" {
		var call struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(message.Params, &call) != nil {
			reply(nil, errors.New("invalid tool call"))
			return
		}
		available, err := g.tools(r.Context(), sandbox)
		if err != nil {
			reply(nil, err)
			return
		}
		if call.Name == "request_mcp_install" {
			var input struct {
				Name      string   `json:"mcp_name"`
				Transport string   `json:"transport"`
				Endpoint  string   `json:"endpoint"`
				Tools     []string `json:"requested_tools"`
				Fields    []string `json:"credential_fields"`
			}
			dec := json.NewDecoder(bytes.NewReader(call.Arguments))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&input); err != nil {
				reply(nil, errors.New("invalid installation arguments"))
				return
			}
			submitted, err := g.control.enqueueInstall(sandbox, &gatewaypb.RequestMCPInstall{OperationId: newID(), SandboxId: sandbox, McpName: input.Name, Transport: input.Transport, Endpoint: input.Endpoint, RequestedTools: input.Tools, CredentialFields: input.Fields})
			if err != nil {
				reply(nil, errors.New(status.Convert(err).Message()))
				return
			}
			text, _ := json.Marshal(map[string]string{"request_id": submitted.RequestId, "status": "pending", "message": "User approval requested. Use new tools only after they appear in your tool list."})
			reply(map[string]any{"content": []map[string]string{{"type": "text", "text": string(text)}}}, nil)
			return
		}
		var tool *gatewayMCPTool
		for i := range available {
			if available[i].Name == call.Name {
				tool = &available[i]
				break
			}
		}
		if tool == nil {
			reply(nil, errors.New("tool is not authorized for this sandbox"))
			return
		}
		var args map[string]json.RawMessage
		if json.Unmarshal(call.Arguments, &args) != nil {
			reply(nil, errors.New("tool arguments must be an object"))
			return
		}
		var schema struct {
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			reply(nil, errors.New("invalid tool schema"))
			return
		}
		changed := false
		for field, value := range args {
			var placeholder string
			if json.Unmarshal(value, &placeholder) != nil || !strings.HasPrefix(placeholder, "$PRIVACY_PROTECT_") {
				continue
			}
			if placeholder != "$PRIVACY_PROTECT_"+field || !gatewayFieldName.MatchString(field) || schema.Properties[field] == nil {
				reply(nil, errors.New("invalid sensitive field placeholder"))
				return
			}
			clients, err := g.clients(r.Context())
			if err != nil {
				reply(nil, err)
				return
			}
			id, name := "", ""
			for _, client := range clients {
				for _, t := range client.Tools {
					// Management returns "search"; /mcp exposes "project-search".
					// Reconstruct the name instead of splitting client names at '-'.
					if client.Config.Name+"-"+t.Name == call.Name {
						id, name = client.Config.ID, client.Config.Name
					}
				}
			}
			if id == "" {
				reply(nil, errors.New("MCP client for tool was not found"))
				return
			}
			secret, err := g.control.lookupSecret(r.Context(), &gatewaypb.ResolveSecret{OperationId: newID(), SandboxId: sandbox, McpId: id, McpName: name, ToolName: call.Name, FieldName: field, Placeholder: placeholder})
			if err != nil {
				reply(nil, errors.New(status.Convert(err).Message()))
				return
			}
			args[field], _ = json.Marshal(string(secret))
			changed = true
		}
		if changed {
			call.Arguments, _ = json.Marshal(args)
			message.Params, _ = json.Marshal(call)
			raw, _ = json.Marshal(message)
		}
	}
	upstream, code, err := g.request(r.Context(), sandbox, raw)
	if err != nil {
		reply(nil, err)
		return
	}
	if code < 200 || code >= 300 {
		http.Error(w, "MCP upstream request failed", code)
		return
	}
	if code == http.StatusAccepted {
		w.WriteHeader(code)
		return
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(upstream, &response) != nil {
		http.Error(w, "invalid MCP response", 502)
		return
	}
	if len(response["error"]) == 0 && (message.Method == "initialize" || message.Method == "tools/list") {
		var result map[string]json.RawMessage
		if json.Unmarshal(response["result"], &result) != nil {
			http.Error(w, "invalid MCP result", 502)
			return
		}
		if message.Method == "initialize" {
			var caps map[string]json.RawMessage
			_ = json.Unmarshal(result["capabilities"], &caps)
			if caps == nil {
				caps = map[string]json.RawMessage{}
			}
			caps["tools"] = json.RawMessage(`{"listChanged":true}`)
			result["capabilities"], _ = json.Marshal(caps)
			sessionID = newID()
			g.mu.Lock()
			g.sessions[sessionID] = &gatewayMCPSession{sandbox: sandbox, changed: make(chan struct{}, 1), closed: make(chan struct{})}
			g.mu.Unlock()
			w.Header().Set("Mcp-Session-Id", sessionID)
		} else {
			var tools []json.RawMessage
			_ = json.Unmarshal(result["tools"], &tools)
			tools = append(tools, json.RawMessage(`{"name":"request_mcp_install","description":"Request user approval to add an MCP. Supply no secrets or mcp_id; use credential_fields for field names. New tools become available after approval and tool-list refresh.","inputSchema":{"type":"object","properties":{"mcp_name":{"type":"string"},"transport":{"type":"string","enum":["http","sse","stdio"]},"endpoint":{"type":"string"},"requested_tools":{"type":"array","items":{"type":"string"}},"credential_fields":{"type":"array","items":{"type":"string"}}},"required":["mcp_name","transport","endpoint","requested_tools"],"additionalProperties":false}}`))
			result["tools"], _ = json.Marshal(tools)
		}
		response["result"], _ = json.Marshal(result)
		upstream, _ = json.Marshal(response)
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(upstream)
}

func (g *gatewayMCP) install(ctx context.Context, r *gatewaypb.MCPInstallApprovalRequest) (string, error) {
	if g.keys[r.SandboxId] == "" {
		return "", status.Error(codes.PermissionDenied, "sandbox has no MCP virtual-key binding")
	}
	id, err := g.register(ctx, r)
	if err != nil {
		return "", err
	}
	// The existing event carries the real client ID to the frontend before
	// waiting for input. APPROVED is a user decision, not tool readiness.
	g.control.broadcastEvent(&gatewaypb.ServerEvent{Body: &gatewaypb.ServerEvent_McpInstallChanged{McpInstallChanged: &gatewaypb.MCPInstallChanged{RequestId: r.RequestId, SandboxId: r.SandboxId, McpId: id, McpName: r.McpName, Change: gatewaypb.MCPInstallChange_MCP_INSTALL_APPROVED}}}, func(p *gatewayPeer) bool { return p.checkSandbox(r.SandboxId) == nil })
	for _, field := range r.CredentialFields {
		if _, err := g.control.lookupSecret(ctx, &gatewaypb.ResolveSecret{OperationId: newID(), SandboxId: r.SandboxId, McpId: id, McpName: r.McpName, FieldName: field, Placeholder: "$PRIVACY_PROTECT_" + field}); err != nil {
			return id, err
		}
	}
	if err := g.grant(ctx, r); err != nil {
		return id, err
	}
	if _, err := g.tools(ctx, r.SandboxId); err != nil {
		return id, err
	}
	g.notify(r.SandboxId)
	return id, nil
}
