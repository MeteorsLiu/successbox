package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	gatewaypb "github.com/MeteorsLiu/successbox/api/gateway/v1"
	"google.golang.org/grpc"
)

func startGatewayControl(ctx context.Context, address, database string, tokens map[string]string, mcp *gatewayMCP) (func(), <-chan error, error) {
	if address == "" {
		return func() {}, nil, nil
	}
	for _, name := range []string{"frontend", "gateway", "agent"} {
		if tokens[name] == "" {
			return nil, nil, fmt.Errorf("--control-%s-token is required when --control-listen is set", name)
		}
	}
	store, err := openGatewayStore(database)
	if err != nil {
		return nil, nil, err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		_ = store.close()
		return nil, nil, fmt.Errorf("listen gateway control: %w", err)
	}
	rpc := grpc.NewServer()
	server := newGatewayServer(store, tokens)
	server.mcp = mcp
	if mcp != nil {
		mcp.control = server
	}
	gatewaypb.RegisterGatewayManagementServiceServer(rpc, server)
	errs := make(chan error, 1)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			rpc.Stop()
			_ = listener.Close()
			_ = store.close()
		})
	}
	go func() {
		<-ctx.Done()
		stop()
	}()
	go func() {
		err := rpc.Serve(listener)
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			select {
			case errs <- err:
			default:
			}
		}
	}()
	return stop, errs, nil
}
