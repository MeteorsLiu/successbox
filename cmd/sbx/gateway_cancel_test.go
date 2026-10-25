package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	gatewaypb "github.com/MeteorsLiu/successbox/api/gateway/v1"
	"google.golang.org/grpc/codes"
)

func testGatewayCancellation(t *testing.T) {
	t.Run("scope permissions idempotence and persistence", func(t *testing.T) {
		server, _ := gatewayTestServer(t)
		front := gatewayTestClient(t, server, "frontend", "front")
		openGatewayTestSession(t, front, "")
		gateA := gatewayTestClient(t, server, "gateway", "gate")
		openGatewayTestSession(t, gateA, "A")
		gateB := gatewayTestClient(t, server, "gateway", "gate")
		openGatewayTestSession(t, gateB, "B")
		control := gatewayTestClient(t, server, "gateway", "gate")
		openGatewayTestSession(t, control, "A")
		agent := gatewayTestClient(t, server, "agent", "agent")
		openGatewayTestSession(t, agent, "A")
		sendGatewayRequest(t, front, "save", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_CreateMcpSecret{CreateMcpSecret: &gatewaypb.CreateMCPSecret{SandboxId: "A", McpId: "saved", FieldName: "api_key", SecretValue: []byte("fixture-secret-retained")}}})
		if gatewayTestResponse(t, front, "save").GetMutationAccepted() == nil {
			t.Fatal("fixture credential was not saved")
		}
		readRequest := func() *gatewaypb.ServerRequest {
			t.Helper()
			for {
				message, err := front.Recv()
				if err != nil {
					t.Fatal(err)
				}
				if message.GetRequest() != nil {
					return message.GetRequest()
				}
			}
		}
		installs := map[string]string{}
		for _, sandbox := range []string{"A", "B"} {
			gate := gateA
			if sandbox == "B" {
				gate = gateB
			}
			sendGatewayRequest(t, gate, "resolve-"+sandbox, &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_ResolveSecret{ResolveSecret: &gatewaypb.ResolveSecret{OperationId: "resolve-" + sandbox, SandboxId: sandbox, McpId: "missing", FieldName: "api_key", Placeholder: "$PRIVACY_PROTECT_api_key"}}})
			if input := readRequest().GetSecretInput(); input == nil || input.SandboxId != sandbox {
				t.Fatal("missing-value request did not retain its sandbox")
			}
			sendGatewayRequest(t, gate, "install-"+sandbox, &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_RequestMcpInstall{RequestMcpInstall: &gatewaypb.RequestMCPInstall{OperationId: "install-" + sandbox, SandboxId: sandbox, McpName: "project", Transport: "http", Endpoint: "https://example.invalid/mcp"}}})
			installs[sandbox] = gatewayTestResponse(t, gate, "install-"+sandbox).GetMcpInstallSubmitted().GetRequestId()
			if approval := readRequest().GetMcpInstallApproval(); approval == nil || approval.RequestId != installs[sandbox] {
				t.Fatal("missing installation approval")
			}
		}
		for _, peer := range []gatewaypb.GatewayManagementService_GatewaySessionClient{front, agent} {
			sendGatewayRequest(t, peer, "forbidden", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_CancelSandboxOperations{CancelSandboxOperations: &gatewaypb.CancelSandboxOperations{SandboxId: "A"}}})
			if gatewayTestResponse(t, peer, "forbidden").GetOperationError().GetCode() != codes.PermissionDenied.String() {
				t.Fatal("a non-gateway client cancelled sandbox operations")
			}
		}
		for _, test := range []struct{ sandbox, code string }{{"B", codes.PermissionDenied.String()}, {"", codes.InvalidArgument.String()}} {
			sendGatewayRequest(t, control, "wrong-scope", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_CancelSandboxOperations{CancelSandboxOperations: &gatewaypb.CancelSandboxOperations{SandboxId: test.sandbox}}})
			if gatewayTestResponse(t, control, "wrong-scope").GetOperationError().GetCode() != test.code {
				t.Fatal("cancellation accepted an invalid sandbox scope")
			}
		}
		for range 2 {
			sendGatewayRequest(t, control, "cancel", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_CancelSandboxOperations{CancelSandboxOperations: &gatewaypb.CancelSandboxOperations{SandboxId: "A"}}})
			if gatewayTestResponse(t, control, "cancel").GetSandboxOperationsCancelled().GetSandboxId() != "A" {
				t.Fatal("cancellation was not acknowledged idempotently")
			}
		}
		if gatewayTestResponse(t, gateA, "resolve-A").GetOperationError().GetCode() != codes.Aborted.String() {
			t.Fatal("A's waiting resolution was not aborted")
		}
		sendGatewayRequest(t, front, "get", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_GetMcpSecret{GetMcpSecret: &gatewaypb.GetMCPSecret{SandboxId: "A", McpId: "saved", FieldName: "api_key"}}})
		if string(gatewayTestResponse(t, front, "get").GetSecretRecord().GetSecretValue()) != "fixture-secret-retained" {
			t.Fatal("cancellation removed saved credentials")
		}
		sendGatewayRequest(t, front, "fill-B", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_CreateMcpSecret{CreateMcpSecret: &gatewaypb.CreateMCPSecret{SandboxId: "B", McpId: "missing", FieldName: "api_key", SecretValue: []byte("fixture-secret-B")}}})
		gatewayTestResponse(t, front, "fill-B")
		if string(gatewayTestResponse(t, gateB, "resolve-B").GetSecretValue().GetValue()) != "fixture-secret-B" {
			t.Fatal("cancelling A disrupted B's resolution")
		}
		for _, sandbox := range []string{"A", "B"} {
			if err := front.Send(&gatewaypb.GatewayClientMessage{CorrelationId: "decide", Body: &gatewaypb.GatewayClientMessage_Response{Response: &gatewaypb.ClientResponse{Body: &gatewaypb.ClientResponse_DecideMcpInstall{DecideMcpInstall: &gatewaypb.DecideMCPInstall{RequestId: installs[sandbox], OperationId: "decide", Approve: false}}}}}); err != nil {
				t.Fatal(err)
			}
			response := gatewayTestResponse(t, front, "decide")
			if sandbox == "A" && response.GetOperationError().GetCode() != codes.NotFound.String() {
				t.Fatal("cancelled installation remained actionable")
			}
			if sandbox == "B" && response.GetMcpInstallDecisionAccepted().GetChange() != gatewaypb.MCPInstallChange_MCP_INSTALL_REJECTED {
				t.Fatal("cancelling A disrupted B's installation")
			}
		}
	})

	t.Run("five minutes from original request", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			server := newGatewayServer(nil, nil)
			defer server.cancelSandboxOperations("")
			request := &gatewaypb.RequestMCPInstall{OperationId: "first", SandboxId: "A", McpName: "project", Transport: "http", Endpoint: "https://example.invalid/mcp"}
			first, err := server.enqueueInstall("A", request)
			if err != nil {
				t.Fatal(err)
			}
			server.mu.Lock()
			pending := server.installs[first.RequestId]
			server.mu.Unlock()
			time.Sleep(4 * time.Minute)
			request.OperationId = "duplicate"
			duplicate, err := server.enqueueInstall("A", request)
			if err != nil || duplicate.RequestId != first.RequestId {
				t.Fatal("duplicate request did not reuse its original deadline")
			}
			time.Sleep(59 * time.Second)
			if pending.ctx.Err() != nil {
				t.Fatal("installation expired before five minutes")
			}
			time.Sleep(time.Second)
			synctest.Wait()
			if context.Cause(pending.ctx) != context.DeadlineExceeded {
				t.Fatal("installation did not expire at its original deadline")
			}
			server.mu.Lock()
			left := len(server.installs)
			server.mu.Unlock()
			if left != 0 {
				t.Fatal("expired installation remains pending")
			}
			next, err := server.enqueueInstall("A", request)
			if err != nil || next.RequestId == first.RequestId {
				t.Fatal("a new request reused an expired approval")
			}
		})
	})

	for _, phase := range []string{"registration", "credential input"} {
		t.Run("interrupt "+phase, func(t *testing.T) {
			server, _ := gatewayTestServer(t)
			started, disconnected := make(chan struct{}), make(chan struct{})
			var registered atomic.Bool
			var grants atomic.Int32
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/mcp/clients":
					clients := []any{}
					if registered.Load() {
						clients = append(clients, map[string]any{"config": map[string]string{"client_id": "fixture-id", "name": "project"}})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"clients": clients, "total_count": len(clients)})
				case "/api/mcp/client":
					_, _ = io.Copy(io.Discard, r.Body)
					close(started)
					if phase == "registration" {
						<-r.Context().Done()
						close(disconnected)
						return
					}
					registered.Store(true)
					_, _ = io.WriteString(w, `{}`)
				default:
					grants.Add(1)
					http.Error(w, "grant must not run", 500)
				}
			}))
			t.Cleanup(backend.Close)
			server.mcp = &gatewayMCP{base: backend.URL, client: backend.Client(), control: server, keys: map[string]string{"A": "fixture-key"}}
			front := gatewayTestClient(t, server, "frontend", "front")
			openGatewayTestSession(t, front, "A")
			control := gatewayTestClient(t, server, "gateway", "gate")
			openGatewayTestSession(t, control, "A")
			sendGatewayRequest(t, control, "install", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_RequestMcpInstall{RequestMcpInstall: &gatewaypb.RequestMCPInstall{OperationId: "install", SandboxId: "A", McpName: "project", Transport: "http", Endpoint: "https://example.invalid/mcp", CredentialFields: []string{"api_key"}}}})
			id := gatewayTestResponse(t, control, "install").GetMcpInstallSubmitted().GetRequestId()
			message, err := front.Recv()
			if err != nil || message.GetRequest().GetMcpInstallApproval().GetRequestId() != id {
				t.Fatal("missing installation approval:", err)
			}
			if err := front.Send(&gatewaypb.GatewayClientMessage{CorrelationId: "approve", Body: &gatewaypb.GatewayClientMessage_Response{Response: &gatewaypb.ClientResponse{Body: &gatewaypb.ClientResponse_DecideMcpInstall{DecideMcpInstall: &gatewaypb.DecideMCPInstall{RequestId: id, OperationId: "approve", Approve: true}}}}}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("registration did not start")
			}
			if phase == "credential input" {
				for {
					message, err := front.Recv()
					if err != nil {
						t.Fatal(err)
					}
					if message.GetRequest().GetSecretInput() != nil {
						break
					}
				}
				// The virtual-time case above verifies the five-minute interval.
				// Here, fire the same timer while real network I/O and a gRPC
				// approval worker are waiting for a credential.
				server.mu.Lock()
				server.installs[id].timer.Reset(time.Millisecond)
				server.mu.Unlock()
			} else {
				sendGatewayRequest(t, control, "cancel", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_CancelSandboxOperations{CancelSandboxOperations: &gatewaypb.CancelSandboxOperations{SandboxId: "A"}}})
				if gatewayTestResponse(t, control, "cancel").GetSandboxOperationsCancelled().GetSandboxId() != "A" {
					t.Fatal("running installation cancellation was not acknowledged")
				}
				select {
				case <-disconnected:
				case <-time.After(5 * time.Second):
					t.Fatal("registration request remained connected after cancellation")
				}
			}
			if gatewayTestResponse(t, front, "approve").GetOperationError().GetCode() != codes.Aborted.String() {
				t.Fatal("interrupted approval did not return ABORTED")
			}
			server.mu.Lock()
			left := len(server.pending) + len(server.installs)
			server.mu.Unlock()
			if left != 0 || grants.Load() != 0 {
				t.Fatal("interrupted installation retained waiters or attempted a grant")
			}
		})
	}
}
