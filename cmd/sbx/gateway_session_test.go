package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gatewaypb "github.com/MeteorsLiu/successbox/api/gateway/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func gatewayTestClient(t *testing.T, server *gatewayServer, kind, token string) gatewaypb.GatewayManagementService_GatewaySessionClient {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rpc := grpc.NewServer()
	gatewaypb.RegisterGatewayManagementServiceServer(rpc, server)
	go func() { _ = rpc.Serve(listener) }()
	t.Cleanup(func() { rpc.Stop(); _ = listener.Close() })
	timeout := 10 * time.Second
	ctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(context.Background(), metadata.Pairs("x-successbox-client", kind, "authorization", "Bearer "+token)), timeout)
	t.Cleanup(cancel)
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	stream, err := gatewaypb.NewGatewayManagementServiceClient(connection).GatewaySession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func openGatewayTestSession(t *testing.T, stream gatewaypb.GatewayManagementService_GatewaySessionClient, sandbox string) {
	t.Helper()
	sendGatewayRequest(t, stream, "open", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_OpenSession{OpenSession: &gatewaypb.OpenSession{SandboxId: sandbox}}})
	if response := gatewayTestResponse(t, stream, "open"); response.GetSessionOpened() == nil {
		t.Fatalf("open response: %v", response)
	}
}

func sendGatewayRequest(t *testing.T, stream gatewaypb.GatewayManagementService_GatewaySessionClient, correlation string, request *gatewaypb.ClientRequest) {
	t.Helper()
	if err := stream.Send(&gatewaypb.GatewayClientMessage{CorrelationId: correlation, Body: &gatewaypb.GatewayClientMessage_Request{Request: request}}); err != nil {
		t.Fatal(err)
	}
}

func gatewayTestResponse(t *testing.T, stream gatewaypb.GatewayManagementService_GatewaySessionClient, correlation string) *gatewaypb.ServerResponse {
	t.Helper()
	for {
		message, err := stream.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if message.GetEvent() != nil {
			if strings.Contains(message.String(), "fixture-secret") {
				t.Fatal("event contains secret value")
			}
			continue
		}
		if message.CorrelationId != correlation || message.GetResponse() == nil {
			t.Fatalf("unexpected response while waiting for %s: %v", correlation, message)
		}
		return message.GetResponse()
	}
}

func gatewayTestServer(t *testing.T) (*gatewayServer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gateway.db")
	store, err := openGatewayStore(path)
	if err != nil {
		t.Fatal(err)
	}
	server := newGatewayServer(store, map[string]string{"frontend": "front", "gateway": "gate", "agent": "agent"})
	t.Cleanup(func() {
		_ = store.close()
	})
	return server, path
}

func TestGatewaySessionResolvesSecretThroughFrontend(t *testing.T) {
	t.Run("CRUD and persistence", func(t *testing.T) {
		server, path := gatewayTestServer(t)
		front := gatewayTestClient(t, server, "frontend", "front")
		openGatewayTestSession(t, front, "A")
		create := &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_CreateMcpSecret{CreateMcpSecret: &gatewaypb.CreateMCPSecret{SandboxId: "A", McpId: "mcp", McpName: "project", FieldName: "api_key", SecretValue: []byte("fixture-secret-one")}}}
		sendGatewayRequest(t, front, "create", create)
		created := gatewayTestResponse(t, front, "create").GetMutationAccepted().GetRecord()
		if created == nil || !created.HasValue || len(created.SecretValue) != 0 || created.CreatedAt == nil {
			t.Fatalf("create metadata: %v", created)
		}
		sendGatewayRequest(t, front, "duplicate", create)
		if code := gatewayTestResponse(t, front, "duplicate").GetOperationError().GetCode(); code != codes.AlreadyExists.String() {
			t.Fatalf("duplicate code = %s", code)
		}
		sendGatewayRequest(t, front, "second-field", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_CreateMcpSecret{CreateMcpSecret: &gatewaypb.CreateMCPSecret{SandboxId: "A", McpId: "mcp", McpName: "project", FieldName: "password", SecretValue: []byte("fixture-secret-password")}}})
		if gatewayTestResponse(t, front, "second-field").GetMutationAccepted() == nil {
			t.Fatal("second field was not created")
		}
		sendGatewayRequest(t, front, "update", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_UpdateMcpSecret{UpdateMcpSecret: &gatewaypb.UpdateMCPSecret{SandboxId: "A", McpId: "mcp", McpName: "renamed", FieldName: "api_key", SecretValue: []byte("fixture-secret-two")}}})
		updated := gatewayTestResponse(t, front, "update").GetMutationAccepted().GetRecord()
		if updated == nil || !updated.CreatedAt.AsTime().Equal(created.CreatedAt.AsTime()) || updated.McpName != "renamed" {
			t.Fatalf("updated metadata: %v", updated)
		}
		sendGatewayRequest(t, front, "list", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_ListMcpSecrets{ListMcpSecrets: &gatewaypb.ListMCPSecrets{SandboxId: "A"}}})
		listed := gatewayTestResponse(t, front, "list").GetSecretList()
		if len(listed.GetRecords()) != 2 {
			t.Fatalf("list = %v", listed)
		}
		for _, r := range listed.Records {
			if r.McpName != "renamed" || len(r.SecretValue) != 0 || !r.HasValue {
				t.Fatalf("list metadata = %v", r)
			}
		}
		sendGatewayRequest(t, front, "get", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_GetMcpSecret{GetMcpSecret: &gatewaypb.GetMCPSecret{SandboxId: "A", McpId: "mcp", FieldName: "api_key"}}})
		if string(gatewayTestResponse(t, front, "get").GetSecretRecord().GetSecretValue()) != "fixture-secret-two" {
			t.Fatal("Get did not return updated value")
		}
		other := gatewayTestClient(t, server, "frontend", "front")
		openGatewayTestSession(t, other, "B")
		sendGatewayRequest(t, other, "list", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_ListMcpSecrets{ListMcpSecrets: &gatewaypb.ListMCPSecrets{SandboxId: "B", McpId: "mcp"}}})
		if len(gatewayTestResponse(t, other, "list").GetSecretList().GetRecords()) != 0 {
			t.Fatal("B can list A records")
		}
		sendGatewayRequest(t, other, "cross", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_GetMcpSecret{GetMcpSecret: &gatewaypb.GetMCPSecret{SandboxId: "A", McpId: "mcp", FieldName: "api_key"}}})
		if gatewayTestResponse(t, other, "cross").GetOperationError().GetCode() != codes.PermissionDenied.String() {
			t.Fatal("cross-sandbox Get was accepted")
		}
		sendGatewayRequest(t, front, "delete", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_DeleteMcpSecret{DeleteMcpSecret: &gatewaypb.DeleteMCPSecret{SandboxId: "A", McpId: "mcp", FieldName: "password"}}})
		if gatewayTestResponse(t, front, "delete").GetMutationAccepted().GetRecord().GetHasValue() {
			t.Fatal("delete retained value")
		}
		sendGatewayRequest(t, front, "delete-missing", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_DeleteMcpSecret{DeleteMcpSecret: &gatewaypb.DeleteMCPSecret{SandboxId: "A", McpId: "mcp", FieldName: "password"}}})
		if gatewayTestResponse(t, front, "delete-missing").GetOperationError().GetCode() != codes.NotFound.String() {
			t.Fatal("delete missing was accepted")
		}
		reopened, err := openGatewayStore(path)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.close()
		saved, err := reopened.get(context.Background(), "A", "mcp", "api_key", false)
		if err != nil || string(saved.GetSecretValue()) != "fixture-secret-two" {
			t.Fatalf("reopen: %v", err)
		}
		rows, err := reopened.db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'`)
		if err != nil {
			t.Fatal(err)
		}
		var tables []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			tables = append(tables, name)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(tables, []string{"sandbox_mcp_secrets"}) {
			t.Fatalf("unexpected tables: %v", tables)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("private database permissions: %v %v", info, err)
		}
	})

	t.Run("concurrent missing values share a prompt", func(t *testing.T) {
		server, _ := gatewayTestServer(t)
		front := gatewayTestClient(t, server, "frontend", "front")
		openGatewayTestSession(t, front, "A")
		gate := gatewayTestClient(t, server, "gateway", "gate")
		openGatewayTestSession(t, gate, "A")
		const count = 8
		for i := 0; i < count; i++ {
			id := fmt.Sprint(i)
			sendGatewayRequest(t, gate, id, &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_ResolveSecret{ResolveSecret: &gatewaypb.ResolveSecret{OperationId: id, SandboxId: "A", McpId: "mcp", McpName: "project", FieldName: "api_key", Placeholder: "$PRIVACY_PROTECT_api_key", TimeoutMs: 5000}}})
		}
		request, err := front.Recv()
		if err != nil || request.GetRequest().GetSecretInput() == nil {
			t.Fatalf("input request: %v %v", request, err)
		}
		key := gatewaySecretKey{"A", "mcp", "api_key"}
		deadline := time.Now().Add(3 * time.Second)
		for {
			server.mu.Lock()
			waiters := len(server.pending[key].waiters)
			pending := len(server.pending)
			server.mu.Unlock()
			if waiters == count {
				if pending != 1 {
					t.Fatalf("pending = %d", pending)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("waiters=%d", waiters)
			}
			time.Sleep(time.Millisecond)
		}
		sendGatewayRequest(t, front, "fill", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_CreateMcpSecret{CreateMcpSecret: &gatewaypb.CreateMCPSecret{SandboxId: "A", McpId: "mcp", McpName: "project", FieldName: "api_key", SecretValue: []byte("fixture-secret-shared")}}})
		if gatewayTestResponse(t, front, "fill").GetMutationAccepted() == nil {
			t.Fatal("Create did not complete")
		}
		seen := map[string]bool{}
		for range count {
			message, err := gate.Recv()
			if err != nil {
				t.Fatal(err)
			}
			value := message.GetResponse().GetSecretValue()
			if value == nil || string(value.Value) != "fixture-secret-shared" || value.OperationId != message.CorrelationId || seen[value.OperationId] {
				t.Fatalf("resolution: %v", message)
			}
			seen[value.OperationId] = true
		}
		sendGatewayRequest(t, gate, "cached", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_ResolveSecret{ResolveSecret: &gatewaypb.ResolveSecret{OperationId: "cached", SandboxId: "A", McpId: "mcp", FieldName: "api_key", Placeholder: "$PRIVACY_PROTECT_api_key"}}})
		if string(gatewayTestResponse(t, gate, "cached").GetSecretValue().GetValue()) != "fixture-secret-shared" {
			t.Fatal("cached resolution failed")
		}
		saved, err := server.store.get(context.Background(), "A", "mcp", "api_key", false)
		if err != nil || saved.LastUsedAt == nil {
			t.Fatal("last_used_at not recorded")
		}
		server.mu.Lock()
		left := len(server.pending)
		server.mu.Unlock()
		if left != 0 {
			t.Fatal("completed waiter remains in memory")
		}
	})

	t.Run("provide cancel timeout and isolation", func(t *testing.T) {
		server, _ := gatewayTestServer(t)
		front := gatewayTestClient(t, server, "frontend", "front")
		openGatewayTestSession(t, front, "A")
		gate := gatewayTestClient(t, server, "gateway", "gate")
		openGatewayTestSession(t, gate, "A")
		for _, action := range []string{"provide", "cancel", "timeout"} {
			timeout := int64(5000)
			if action == "timeout" {
				timeout = 100
			}
			sendGatewayRequest(t, gate, action, &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_ResolveSecret{ResolveSecret: &gatewaypb.ResolveSecret{OperationId: action, SandboxId: "A", McpId: action, FieldName: "api_key", Placeholder: "$PRIVACY_PROTECT_api_key", TimeoutMs: timeout}}})
			var input *gatewaypb.SecretInputRequest
			for input == nil {
				m, e := front.Recv()
				if e != nil {
					t.Fatal(e)
				}
				input = m.GetRequest().GetSecretInput()
			}
			if action == "provide" {
				err := front.Send(&gatewaypb.GatewayClientMessage{CorrelationId: action, Body: &gatewaypb.GatewayClientMessage_Response{Response: &gatewaypb.ClientResponse{Body: &gatewaypb.ClientResponse_ProvideSecret{ProvideSecret: &gatewaypb.ProvideSecret{OperationId: input.OperationId, SandboxId: "A", McpId: action, FieldName: "api_key", SecretValue: []byte("fixture-secret-provided")}}}}})
				if err != nil {
					t.Fatal(err)
				}
				if gatewayTestResponse(t, front, action).GetMutationAccepted() == nil {
					t.Fatal("provide did not persist")
				}
				if string(gatewayTestResponse(t, gate, action).GetSecretValue().GetValue()) != "fixture-secret-provided" {
					t.Fatal("provide failed")
				}
			} else {
				if action == "cancel" {
					if err := front.Send(&gatewaypb.GatewayClientMessage{CorrelationId: action, Body: &gatewaypb.GatewayClientMessage_Response{Response: &gatewaypb.ClientResponse{Body: &gatewaypb.ClientResponse_CancelSecret{CancelSecret: &gatewaypb.CancelSecret{OperationId: input.OperationId}}}}}); err != nil {
						t.Fatal(err)
					}
					if gatewayTestResponse(t, front, action).GetOperationError().GetCode() != codes.Aborted.String() {
						t.Fatal("cancel was not acknowledged")
					}
				}
				if gatewayTestResponse(t, gate, action).GetOperationError().GetCode() != codes.Aborted.String() {
					t.Fatal("pending operation did not end")
				}
			}
		}
		sendGatewayRequest(t, gate, "cross", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_ResolveSecret{ResolveSecret: &gatewaypb.ResolveSecret{OperationId: "cross", SandboxId: "B", McpId: "provide", FieldName: "api_key", Placeholder: "$PRIVACY_PROTECT_api_key"}}})
		if gatewayTestResponse(t, gate, "cross").GetOperationError().GetCode() != codes.PermissionDenied.String() {
			t.Fatal("cross-sandbox Resolve accepted")
		}
		sendGatewayRequest(t, gate, "crud", &gatewaypb.ClientRequest{Body: &gatewaypb.ClientRequest_GetMcpSecret{GetMcpSecret: &gatewaypb.GetMCPSecret{SandboxId: "A", McpId: "provide", FieldName: "api_key"}}})
		if gatewayTestResponse(t, gate, "crud").GetOperationError().GetCode() != codes.PermissionDenied.String() {
			t.Fatal("gateway received frontend CRUD access")
		}
		bad := gatewayTestClient(t, server, "gateway", "wrong")
		_, err := bad.Recv()
		if status.Code(err) != codes.Unauthenticated {
			t.Fatalf("wrong token: %v", err)
		}
	})
}
