package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	gatewaypb "github.com/MeteorsLiu/successbox/api/gateway/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type gatewayServer struct {
	gatewaypb.UnimplementedGatewayManagementServiceServer
	store *gatewayStore
	// mu orders database changes with waiter registration. For example, a
	// frontend Create cannot commit between a missing-value lookup and enqueue.
	// No stream send takes place while mu is held.
	mu       sync.Mutex
	peers    map[*gatewayPeer]struct{}
	pending  map[gatewaySecretKey]*gatewayPendingSecret
	installs map[string]*gatewayPendingInstall
	tokens   map[string]string
	mcp      *gatewayMCP
}

type gatewayPendingInstall struct {
	request *gatewaypb.MCPInstallApprovalRequest
	ctx     context.Context
	cancel  context.CancelCauseFunc
	timer   *time.Timer
	// done is non-nil only while an approval worker is running. Cancellation
	// waits for that worker outside the server lock before acknowledging.
	done chan struct{}
}

type gatewaySecretKey struct{ sandbox, mcp, field string }

type gatewayPendingSecret struct {
	request *gatewaypb.SecretInputRequest
	waiters map[chan secretResult]struct{}
}

type gatewayPeer struct {
	server    *gatewayServer
	kind      string
	sandboxID string
	ctx       context.Context
	stream    grpc.BidiStreamingServer[gatewaypb.GatewayClientMessage, gatewaypb.GatewayServerMessage]
	mu        sync.Mutex
	workers   sync.WaitGroup
}

type secretResult struct {
	value []byte
	err   error
}

var gatewayFieldName = regexp.MustCompile(`^[a-z0-9_]+$`)

func newGatewayServer(store *gatewayStore, tokens map[string]string) *gatewayServer {
	return &gatewayServer{store: store, peers: make(map[*gatewayPeer]struct{}), pending: make(map[gatewaySecretKey]*gatewayPendingSecret), installs: make(map[string]*gatewayPendingInstall), tokens: tokens}
}

func newID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw[:])
}

func (s *gatewayServer) GatewaySession(stream grpc.BidiStreamingServer[gatewaypb.GatewayClientMessage, gatewaypb.GatewayServerMessage]) error {
	kind, err := authenticatedGatewayClient(stream.Context(), s.tokens)
	if err != nil {
		return err
	}
	message, err := stream.Recv()
	if err != nil {
		return err
	}
	open := message.GetRequest().GetOpenSession()
	if open == nil {
		return status.Error(codes.InvalidArgument, "first gateway message must open a session")
	}
	if kind != "frontend" && open.SandboxId == "" {
		return status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	ctx, cancel := context.WithCancel(stream.Context())
	peer := &gatewayPeer{server: s, kind: kind, sandboxID: open.SandboxId, ctx: ctx, stream: stream}
	if err := peer.accepted(message.CorrelationId, &gatewaypb.ServerResponse{Body: &gatewaypb.ServerResponse_SessionOpened{SessionOpened: &gatewaypb.SessionOpened{SessionId: newID(), SandboxId: peer.sandboxID}}}); err != nil {
		cancel()
		return err
	}
	s.mu.Lock()
	s.peers[peer] = struct{}{}
	s.mu.Unlock()
	defer func() {
		cancel()
		peer.workers.Wait()
		s.mu.Lock()
		delete(s.peers, peer)
		s.mu.Unlock()
	}()
	for {
		message, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if message.GetCorrelationId() == "" {
			_ = peer.error("", codes.InvalidArgument, "correlation_id is required")
			continue
		}
		if request := message.GetRequest(); request != nil {
			s.handleRequest(peer, message.CorrelationId, request)
		} else if response := message.GetResponse(); response != nil {
			s.handleResponse(peer, message.CorrelationId, response)
		} else {
			_ = peer.error(message.CorrelationId, codes.InvalidArgument, "gateway message body is required")
		}
	}
}

func authenticatedGatewayClient(ctx context.Context, tokens map[string]string) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated, "gateway client metadata is required")
	}
	kind := firstMetadata(md, "x-successbox-client")
	token := strings.TrimPrefix(firstMetadata(md, "authorization"), "Bearer ")
	if kind == "" || token == "" || tokens[kind] == "" || tokens[kind] != token {
		return "", status.Error(codes.Unauthenticated, "invalid gateway client credentials")
	}
	if kind != "frontend" && kind != "gateway" && kind != "agent" {
		return "", status.Error(codes.PermissionDenied, "unsupported gateway client")
	}
	return kind, nil
}

func firstMetadata(md metadata.MD, key string) string {
	values := md.Get(key)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (p *gatewayPeer) checkSandbox(id string) error {
	if id == "" {
		return status.Error(codes.InvalidArgument, "sandbox_id is required")
	}
	if p.sandboxID != id && !(p.kind == "frontend" && p.sandboxID == "") {
		return status.Error(codes.PermissionDenied, "sandbox does not match this session")
	}
	return nil
}

func (s *gatewayServer) handleRequest(peer *gatewayPeer, correlation string, request *gatewaypb.ClientRequest) {
	if cancel := request.GetCancelSandboxOperations(); cancel != nil {
		if peer.kind != "gateway" {
			_ = peer.error(correlation, codes.PermissionDenied, "only the host gateway may cancel sandbox operations")
			return
		}
		if err := peer.checkSandbox(cancel.SandboxId); err != nil {
			_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
			return
		}
		s.cancelSandboxOperations(cancel.SandboxId)
		_ = peer.accepted(correlation, &gatewaypb.ServerResponse{Body: &gatewaypb.ServerResponse_SandboxOperationsCancelled{SandboxOperationsCancelled: &gatewaypb.SandboxOperationsCancelled{SandboxId: cancel.SandboxId}}})
		return
	}
	if resume := request.GetResumeSession(); resume != nil {
		if peer.kind != "frontend" {
			_ = peer.error(correlation, codes.PermissionDenied, "only the frontend may replay pending requests")
			return
		}
		if resume.ReplayPending {
			s.replayPending(peer)
		}
		return
	}
	if resolve := request.GetResolveSecret(); resolve != nil {
		if peer.kind != "gateway" {
			_ = peer.error(correlation, codes.PermissionDenied, "only the gateway may resolve secrets")
			return
		}
		peer.workers.Add(1)
		go func() {
			defer peer.workers.Done()
			s.resolveSecret(peer, correlation, resolve)
		}()
		return
	}
	if install := request.GetRequestMcpInstall(); install != nil {
		if peer.kind != "agent" && peer.kind != "gateway" {
			_ = peer.error(correlation, codes.PermissionDenied, "only an agent adapter or gateway may request installation")
			return
		}
		s.submitInstall(peer, correlation, install)
		return
	}
	if peer.kind != "frontend" {
		_ = peer.error(correlation, codes.PermissionDenied, "only the frontend may manage secrets")
		return
	}
	var record *gatewaypb.MCPSecretRecord
	var change gatewaypb.MCPSecretChange
	switch body := request.Body.(type) {
	case *gatewaypb.ClientRequest_CreateMcpSecret:
		v := body.CreateMcpSecret
		record = &gatewaypb.MCPSecretRecord{SandboxId: v.GetSandboxId(), McpId: v.GetMcpId(), McpName: v.GetMcpName(), FieldName: v.GetFieldName(), SecretValue: v.GetSecretValue()}
		change = gatewaypb.MCPSecretChange_MCP_SECRET_CREATED
	case *gatewaypb.ClientRequest_UpdateMcpSecret:
		v := body.UpdateMcpSecret
		record = &gatewaypb.MCPSecretRecord{SandboxId: v.GetSandboxId(), McpId: v.GetMcpId(), McpName: v.GetMcpName(), FieldName: v.GetFieldName(), SecretValue: v.GetSecretValue()}
		change = gatewaypb.MCPSecretChange_MCP_SECRET_UPDATED
	case *gatewaypb.ClientRequest_DeleteMcpSecret:
		v := body.DeleteMcpSecret
		record = &gatewaypb.MCPSecretRecord{SandboxId: v.GetSandboxId(), McpId: v.GetMcpId(), FieldName: v.GetFieldName()}
		change = gatewaypb.MCPSecretChange_MCP_SECRET_DELETED
	case *gatewaypb.ClientRequest_GetMcpSecret:
		v := body.GetMcpSecret
		if err := peer.checkSandbox(v.GetSandboxId()); err != nil {
			_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
			return
		}
		if v.GetMcpId() == "" || !gatewayFieldName.MatchString(v.GetFieldName()) {
			_ = peer.error(correlation, codes.InvalidArgument, "MCP ID and valid field_name are required")
			return
		}
		r, err := s.store.get(peer.ctx, v.SandboxId, v.McpId, v.FieldName, false)
		if err != nil {
			_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
			return
		}
		_ = peer.accepted(correlation, &gatewaypb.ServerResponse{Body: &gatewaypb.ServerResponse_SecretRecord{SecretRecord: r}})
		return
	case *gatewaypb.ClientRequest_ListMcpSecrets:
		v := body.ListMcpSecrets
		if err := peer.checkSandbox(v.GetSandboxId()); err != nil {
			_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
			return
		}
		records, err := s.store.list(peer.ctx, v.SandboxId, v.McpId)
		if err != nil {
			_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
			return
		}
		_ = peer.accepted(correlation, &gatewaypb.ServerResponse{Body: &gatewaypb.ServerResponse_SecretList{SecretList: &gatewaypb.MCPSecretList{Records: records}}})
		return
	default:
		_ = peer.error(correlation, codes.InvalidArgument, "unsupported gateway request")
		return
	}
	s.mutateSecret(peer, correlation, change, record, "")
}

func (s *gatewayServer) mutateSecret(peer *gatewayPeer, correlation string, change gatewaypb.MCPSecretChange, record *gatewaypb.MCPSecretRecord, operation string) {
	if err := peer.checkSandbox(record.SandboxId); err != nil {
		_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
		return
	}
	if record.McpId == "" || !gatewayFieldName.MatchString(record.FieldName) || (change != gatewaypb.MCPSecretChange_MCP_SECRET_DELETED && len(record.SecretValue) == 0) {
		_ = peer.error(correlation, codes.InvalidArgument, "MCP ID, valid field_name and nonempty value are required")
		return
	}
	key := gatewaySecretKey{record.SandboxId, record.McpId, record.FieldName}
	s.mu.Lock()
	pending := s.pending[key]
	if operation != "" && (pending == nil || pending.request.OperationId != operation) {
		s.mu.Unlock()
		_ = peer.error(correlation, codes.NotFound, "pending secret request not found")
		return
	}
	if change == gatewaypb.MCPSecretChange_MCP_SECRET_DELETED && pending != nil {
		s.mu.Unlock()
		_ = peer.error(correlation, codes.FailedPrecondition, "cancel pending resolution before deleting a secret")
		return
	}
	// ProvideSecret is an answer to a pending request; explicit CRUD retains
	// Create's AlreadyExists and Update's NotFound behavior.
	if operation != "" {
		_, err := s.store.get(peer.ctx, record.SandboxId, record.McpId, record.FieldName, false)
		if err == nil {
			change = gatewaypb.MCPSecretChange_MCP_SECRET_UPDATED
		} else if status.Code(err) != codes.NotFound {
			s.mu.Unlock()
			_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
			return
		}
	}
	metadata, err := s.store.mutate(peer.ctx, change, record)
	if err == nil && pending != nil {
		delete(s.pending, key)
		for waiter := range pending.waiters {
			waiter <- secretResult{value: append([]byte(nil), record.SecretValue...)}
		}
	}
	s.mu.Unlock()
	if err != nil {
		_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
		return
	}
	_ = peer.accepted(correlation, &gatewaypb.ServerResponse{Body: &gatewaypb.ServerResponse_MutationAccepted{MutationAccepted: &gatewaypb.MCPSecretMutationAccepted{Change: change, Record: metadata}}})
	s.broadcastEvent(&gatewaypb.ServerEvent{Body: &gatewaypb.ServerEvent_SecretChanged{SecretChanged: &gatewaypb.MCPSecretChanged{Change: change, Record: metadata}}}, func(p *gatewayPeer) bool {
		return p.kind == "frontend" && (p.sandboxID == "" || p.sandboxID == record.SandboxId)
	})
}

func (s *gatewayServer) handleResponse(peer *gatewayPeer, correlation string, response *gatewaypb.ClientResponse) {
	if peer.kind != "frontend" {
		_ = peer.error(correlation, codes.PermissionDenied, "only the frontend may answer input requests")
		return
	}
	switch body := response.Body.(type) {
	case *gatewaypb.ClientResponse_ProvideSecret:
		v := body.ProvideSecret
		if v.GetOperationId() == "" {
			_ = peer.error(correlation, codes.InvalidArgument, "operation_id is required")
			return
		}
		s.mutateSecret(peer, correlation, gatewaypb.MCPSecretChange_MCP_SECRET_CREATED, &gatewaypb.MCPSecretRecord{SandboxId: v.SandboxId, McpId: v.McpId, McpName: v.McpName, FieldName: v.FieldName, SecretValue: v.SecretValue}, v.OperationId)
	case *gatewaypb.ClientResponse_CancelSecret:
		s.mu.Lock()
		var key gatewaySecretKey
		var pending *gatewayPendingSecret
		for k, p := range s.pending {
			if p.request.OperationId == body.CancelSecret.GetOperationId() && peer.checkSandbox(k.sandbox) == nil {
				key, pending = k, p
				break
			}
		}
		if pending != nil {
			delete(s.pending, key)
			for waiter := range pending.waiters {
				waiter <- secretResult{err: status.Error(codes.Aborted, "secret request cancelled")}
			}
		}
		s.mu.Unlock()
		if pending == nil {
			_ = peer.error(correlation, codes.NotFound, "pending secret request not found")
		} else {
			// No persistent row changed. The existing OperationError envelope
			// reports cancellation without inventing a secret mutation event.
			_ = peer.error(correlation, codes.Aborted, "secret request cancelled")
		}
	case *gatewaypb.ClientResponse_DecideMcpInstall:
		v := body.DecideMcpInstall
		s.mu.Lock()
		pending := s.installs[v.GetRequestId()]
		if pending == nil || peer.checkSandbox(pending.request.SandboxId) != nil {
			s.mu.Unlock()
			_ = peer.error(correlation, codes.NotFound, "pending installation not found")
			return
		}
		request := pending.request
		if pending.ctx.Err() != nil {
			s.mu.Unlock()
			_ = peer.error(correlation, codes.Aborted, "installation was cancelled")
			return
		}
		if v.Approve {
			if s.mcp == nil || pending.done != nil {
				s.mu.Unlock()
				_ = peer.error(correlation, codes.FailedPrecondition, "MCP adapter unavailable or installation already in progress")
				return
			}
			done := make(chan struct{})
			pending.done = done
			s.mu.Unlock()
			peer.workers.Add(1)
			go func() {
				defer peer.workers.Done()
				ctx, cancel := context.WithCancel(pending.ctx)
				stop := context.AfterFunc(peer.ctx, cancel)
				defer stop()
				defer cancel()
				id, err := s.mcp.install(ctx, request)
				s.mu.Lock()
				active := s.installs[request.RequestId] == pending && pending.ctx.Err() == nil
				if s.installs[request.RequestId] == pending && (err == nil || !active) {
					delete(s.installs, request.RequestId)
					pending.timer.Stop()
					pending.cancel(nil)
				}
				pending.done = nil
				// Signal completion of installation I/O before sending replies.
				// A frontend that stopped reading must not hold up cancellation.
				close(done)
				s.mu.Unlock()
				if !active {
					_ = peer.error(correlation, codes.Aborted, "installation was cancelled")
					return
				}
				if err != nil {
					_ = peer.error(correlation, codes.FailedPrecondition, err.Error())
					s.broadcastEvent(&gatewaypb.ServerEvent{Body: &gatewaypb.ServerEvent_McpInstallChanged{McpInstallChanged: &gatewaypb.MCPInstallChanged{RequestId: request.RequestId, SandboxId: request.SandboxId, McpId: id, McpName: request.McpName, Change: gatewaypb.MCPInstallChange_MCP_INSTALL_FAILED, Reason: err.Error()}}}, func(p *gatewayPeer) bool { return p.checkSandbox(request.SandboxId) == nil })
					return
				}
				_ = peer.accepted(correlation, &gatewaypb.ServerResponse{Body: &gatewaypb.ServerResponse_McpInstallDecisionAccepted{McpInstallDecisionAccepted: &gatewaypb.MCPInstallDecisionAccepted{RequestId: request.RequestId, Change: gatewaypb.MCPInstallChange_MCP_INSTALL_APPROVED}}})
			}()
			return
		}
		pending.timer.Stop()
		pending.cancel(context.Canceled)
		done := pending.done
		if done == nil {
			delete(s.installs, request.RequestId)
		}
		s.mu.Unlock()
		if done != nil {
			<-done
		}
		_ = peer.accepted(correlation, &gatewaypb.ServerResponse{Body: &gatewaypb.ServerResponse_McpInstallDecisionAccepted{McpInstallDecisionAccepted: &gatewaypb.MCPInstallDecisionAccepted{RequestId: request.RequestId, Change: gatewaypb.MCPInstallChange_MCP_INSTALL_REJECTED}}})
		s.broadcastEvent(&gatewaypb.ServerEvent{Body: &gatewaypb.ServerEvent_McpInstallChanged{McpInstallChanged: &gatewaypb.MCPInstallChanged{RequestId: request.RequestId, SandboxId: request.SandboxId, McpName: request.McpName, Change: gatewaypb.MCPInstallChange_MCP_INSTALL_REJECTED, Reason: v.DecisionReason}}}, func(p *gatewayPeer) bool { return p.checkSandbox(request.SandboxId) == nil })
	default:
		_ = peer.error(correlation, codes.InvalidArgument, "unsupported gateway response")
	}
}

func (s *gatewayServer) resolveSecret(peer *gatewayPeer, correlation string, request *gatewaypb.ResolveSecret) {
	if err := peer.checkSandbox(request.SandboxId); err != nil {
		_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
		return
	}
	value, err := s.lookupSecret(peer.ctx, request)
	if err != nil {
		_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
		return
	}
	_ = peer.accepted(correlation, &gatewaypb.ServerResponse{Body: &gatewaypb.ServerResponse_SecretValue{SecretValue: &gatewaypb.SecretValue{OperationId: request.OperationId, Value: value}}})
}

func (s *gatewayServer) lookupSecret(ctx context.Context, request *gatewaypb.ResolveSecret) ([]byte, error) {
	if request.OperationId == "" || request.SandboxId == "" || request.McpId == "" || !gatewayFieldName.MatchString(request.FieldName) || request.Placeholder != "$PRIVACY_PROTECT_"+request.FieldName || request.TimeoutMs < 0 || request.TimeoutMs > int64((1<<63-1)/time.Millisecond) {
		return nil, status.Error(codes.InvalidArgument, "invalid secret lookup")
	}
	cancel := func() {}
	if request.TimeoutMs > 0 {
		ctx, cancel = context.WithTimeout(ctx, time.Duration(request.TimeoutMs)*time.Millisecond)
	}
	defer cancel()
	key := gatewaySecretKey{request.SandboxId, request.McpId, request.FieldName}
	s.mu.Lock()
	record, err := s.store.get(ctx, request.SandboxId, request.McpId, request.FieldName, true)
	if err == nil {
		s.mu.Unlock()
		value := record.SecretValue
		record.SecretValue = nil
		s.broadcastEvent(&gatewaypb.ServerEvent{Body: &gatewaypb.ServerEvent_SecretChanged{SecretChanged: &gatewaypb.MCPSecretChanged{Change: gatewaypb.MCPSecretChange_MCP_SECRET_USED, Record: record}}}, func(p *gatewayPeer) bool { return p.kind == "frontend" && p.checkSandbox(request.SandboxId) == nil })
		return value, nil
	}
	if status.Code(err) != codes.NotFound {
		s.mu.Unlock()
		return nil, err
	}
	pending := s.pending[key]
	fresh := pending == nil
	if fresh {
		pending = &gatewayPendingSecret{request: &gatewaypb.SecretInputRequest{OperationId: newID(), SandboxId: request.SandboxId, McpId: request.McpId, McpName: request.McpName, FieldName: request.FieldName, Placeholder: request.Placeholder}, waiters: make(map[chan secretResult]struct{})}
		s.pending[key] = pending
	}
	wait := make(chan secretResult, 1)
	pending.waiters[wait] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(pending.waiters, wait)
		if s.pending[key] == pending && len(pending.waiters) == 0 {
			delete(s.pending, key)
		}
		s.mu.Unlock()
	}()
	if fresh {
		s.broadcast(&gatewaypb.ServerRequest{Body: &gatewaypb.ServerRequest_SecretInput{SecretInput: pending.request}}, func(p *gatewayPeer) bool { return p.kind == "frontend" && p.checkSandbox(request.SandboxId) == nil })
	}
	select {
	case result := <-wait:
		return result.value, result.err
	case <-ctx.Done():
		return nil, status.Error(codes.Aborted, "secret resolution ended before a value was provided")
	}
}

func (s *gatewayServer) submitInstall(peer *gatewayPeer, correlation string, request *gatewaypb.RequestMCPInstall) {
	if err := peer.checkSandbox(request.SandboxId); err != nil {
		_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
		return
	}
	submitted, err := s.enqueueInstall(peer.sandboxID, request)
	if err != nil {
		_ = peer.error(correlation, status.Code(err), status.Convert(err).Message())
		return
	}
	_ = peer.accepted(correlation, &gatewaypb.ServerResponse{Body: &gatewaypb.ServerResponse_McpInstallSubmitted{McpInstallSubmitted: submitted}})
}

func (s *gatewayServer) enqueueInstall(sandbox string, request *gatewaypb.RequestMCPInstall) (*gatewaypb.MCPInstallSubmitted, error) {
	if sandbox == "" || request.SandboxId != sandbox {
		return nil, status.Error(codes.PermissionDenied, "sandbox does not match this request")
	}
	if request.OperationId == "" || request.McpName == "" || request.Endpoint == "" || (request.Transport != "http" && request.Transport != "sse" && request.Transport != "stdio") {
		return nil, status.Error(codes.InvalidArgument, "installation requires operation_id, name, transport and endpoint")
	}
	if request.Transport != "stdio" {
		u, err := url.Parse(request.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
			return nil, status.Error(codes.InvalidArgument, "installation requires an HTTP endpoint without embedded credentials")
		}
	}
	for _, field := range request.CredentialFields {
		if !gatewayFieldName.MatchString(field) {
			return nil, status.Error(codes.InvalidArgument, "invalid credential field name")
		}
	}
	approval := &gatewaypb.MCPInstallApprovalRequest{SandboxId: sandbox, McpName: request.McpName, Transport: request.Transport, Endpoint: request.Endpoint, RequestedTools: request.RequestedTools, CredentialFields: request.CredentialFields}
	s.mu.Lock()
	fresh := true
	for _, existing := range s.installs {
		candidate := proto.Clone(existing.request).(*gatewaypb.MCPInstallApprovalRequest)
		candidate.RequestId = ""
		if proto.Equal(candidate, approval) {
			if existing.ctx.Err() != nil {
				s.mu.Unlock()
				return nil, status.Error(codes.Aborted, "installation cancellation is in progress")
			}
			approval, fresh = existing.request, false
			break
		}
	}
	if fresh {
		approval.RequestId = newID()
		ctx, cancel := context.WithCancelCause(context.Background())
		pending := &gatewayPendingInstall{request: approval, ctx: ctx, cancel: cancel}
		s.installs[approval.RequestId] = pending
		// The deadline belongs to the original request, not an approval stream.
		// A duplicate request or a reconnect must not restart these five minutes.
		pending.timer = time.AfterFunc(5*time.Minute, func() {
			s.mu.Lock()
			if s.installs[approval.RequestId] != pending || pending.ctx.Err() != nil {
				s.mu.Unlock()
				return
			}
			pending.cancel(context.DeadlineExceeded)
			done := pending.done
			if done == nil {
				delete(s.installs, approval.RequestId)
			}
			s.mu.Unlock()
			if done != nil {
				<-done
			}
			s.broadcastEvent(&gatewaypb.ServerEvent{Body: &gatewaypb.ServerEvent_McpInstallChanged{McpInstallChanged: &gatewaypb.MCPInstallChanged{RequestId: approval.RequestId, SandboxId: sandbox, McpName: approval.McpName, Change: gatewaypb.MCPInstallChange_MCP_INSTALL_CANCELLED, Reason: "installation request expired"}}}, func(p *gatewayPeer) bool { return p.checkSandbox(sandbox) == nil })
		})
	}
	s.mu.Unlock()
	if fresh {
		s.broadcast(&gatewaypb.ServerRequest{Body: &gatewaypb.ServerRequest_McpInstallApproval{McpInstallApproval: approval}}, func(p *gatewayPeer) bool { return p.kind == "frontend" && p.checkSandbox(sandbox) == nil })
	}
	return &gatewaypb.MCPInstallSubmitted{OperationId: request.OperationId, RequestId: approval.RequestId, Change: gatewaypb.MCPInstallChange_MCP_INSTALL_REQUESTED}, nil
}

// An empty sandbox is used only by the control service's shutdown path.
// RPC callers must supply their bound sandbox ID before entering this method.
func (s *gatewayServer) cancelSandboxOperations(sandbox string) {
	s.mu.Lock()
	var cancelled []*gatewaypb.MCPInstallApprovalRequest
	var workers []chan struct{}
	for id, pending := range s.installs {
		if sandbox != "" && pending.request.SandboxId != sandbox {
			continue
		}
		pending.timer.Stop()
		if pending.ctx.Err() == nil {
			pending.cancel(context.Canceled)
			cancelled = append(cancelled, pending.request)
		}
		if pending.done != nil {
			workers = append(workers, pending.done)
		} else {
			delete(s.installs, id)
		}
	}
	for key, pending := range s.pending {
		if sandbox != "" && key.sandbox != sandbox {
			continue
		}
		delete(s.pending, key)
		for waiter := range pending.waiters {
			waiter <- secretResult{err: status.Error(codes.Aborted, "sandbox operations cancelled")}
		}
	}
	s.mu.Unlock()
	for _, done := range workers {
		<-done
	}
	if sandbox == "" {
		return
	}
	for _, request := range cancelled {
		s.broadcastEvent(&gatewaypb.ServerEvent{Body: &gatewaypb.ServerEvent_McpInstallChanged{McpInstallChanged: &gatewaypb.MCPInstallChanged{RequestId: request.RequestId, SandboxId: sandbox, McpName: request.McpName, Change: gatewaypb.MCPInstallChange_MCP_INSTALL_CANCELLED, Reason: "sandbox operations cancelled"}}}, func(p *gatewayPeer) bool { return p.checkSandbox(sandbox) == nil })
	}
}

func (s *gatewayServer) replayPending(peer *gatewayPeer) {
	s.mu.Lock()
	var requests []*gatewaypb.ServerRequest
	for _, pending := range s.pending {
		if peer.checkSandbox(pending.request.SandboxId) == nil {
			requests = append(requests, &gatewaypb.ServerRequest{Body: &gatewaypb.ServerRequest_SecretInput{SecretInput: pending.request}})
		}
	}
	for _, install := range s.installs {
		if install.ctx.Err() == nil && peer.checkSandbox(install.request.SandboxId) == nil {
			requests = append(requests, &gatewaypb.ServerRequest{Body: &gatewaypb.ServerRequest_McpInstallApproval{McpInstallApproval: install.request}})
		}
	}
	s.mu.Unlock()
	for _, request := range requests {
		_ = peer.send(&gatewaypb.GatewayServerMessage{Body: &gatewaypb.GatewayServerMessage_Request{Request: request}})
	}
}

func (s *gatewayServer) broadcast(body *gatewaypb.ServerRequest, allowed func(*gatewayPeer) bool) {
	s.mu.Lock()
	var peers []*gatewayPeer
	for peer := range s.peers {
		if allowed(peer) {
			peers = append(peers, peer)
		}
	}
	s.mu.Unlock()
	for _, peer := range peers {
		_ = peer.send(&gatewaypb.GatewayServerMessage{Body: &gatewaypb.GatewayServerMessage_Request{Request: body}})
	}
}

func (s *gatewayServer) broadcastEvent(body *gatewaypb.ServerEvent, allowed func(*gatewayPeer) bool) {
	s.mu.Lock()
	var peers []*gatewayPeer
	for peer := range s.peers {
		if allowed(peer) {
			peers = append(peers, peer)
		}
	}
	s.mu.Unlock()
	for _, peer := range peers {
		_ = peer.send(&gatewaypb.GatewayServerMessage{EventId: newID(), Body: &gatewaypb.GatewayServerMessage_Event{Event: body}})
	}
}

func (p *gatewayPeer) send(message *gatewaypb.GatewayServerMessage) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stream.Send(message)
}

func (p *gatewayPeer) accepted(correlation string, body *gatewaypb.ServerResponse) error {
	return p.send(&gatewaypb.GatewayServerMessage{CorrelationId: correlation, Body: &gatewaypb.GatewayServerMessage_Response{Response: body}})
}

func (p *gatewayPeer) error(correlation string, code codes.Code, message string) error {
	return p.accepted(correlation, &gatewaypb.ServerResponse{Body: &gatewaypb.ServerResponse_OperationError{OperationError: &gatewaypb.OperationError{OperationId: correlation, Code: code.String(), Message: message}}})
}
