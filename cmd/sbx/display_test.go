package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	pb "github.com/MeteorsLiu/successbox/api/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type displayFixture struct {
	pb.UnimplementedSandboxServiceServer
	handle func(grpc.BidiStreamingServer[pb.DisplayInput, pb.DisplayOutput]) error
}

func (s displayFixture) Display(stream grpc.BidiStreamingServer[pb.DisplayInput, pb.DisplayOutput]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetConnect().GetId() != "test-sandbox" {
		return status.Error(codes.InvalidArgument, "missing sandbox selection")
	}
	return s.handle(stream)
}

func TestForwardDisplay(t *testing.T) {
	// Test the transport with arbitrary binary data, not a desktop server.
	// Chunk boundaries in gRPC must not change the RDP byte stream.
	data := bytes.Repeat([]byte{0, 255, 1, 0, 128, 13, 10, 42}, 65536)
	for _, test := range []struct {
		name      string
		wantError string
		echo      bool
		handle    func(grpc.BidiStreamingServer[pb.DisplayInput, pb.DisplayOutput]) error
	}{
		{
			name: "binary stream", echo: true,
			handle: func(stream grpc.BidiStreamingServer[pb.DisplayInput, pb.DisplayOutput]) error {
				if err := stream.Send(&pb.DisplayOutput{Payload: &pb.DisplayOutput_Ready{Ready: &pb.DisplayReady{Protocol: pb.DisplayProtocol_DISPLAY_PROTOCOL_RDP}}}); err != nil {
					return err
				}
				received := 0
				for received < len(data) {
					message, err := stream.Recv()
					if err != nil {
						return err
					}
					payload, ok := message.Payload.(*pb.DisplayInput_Data)
					if !ok {
						return status.Error(codes.InvalidArgument, "expected data")
					}
					received += len(payload.Data)
					if err := stream.Send(&pb.DisplayOutput{Payload: &pb.DisplayOutput_Data{Data: payload.Data}}); err != nil {
						return err
					}
				}
				return nil
			},
		},
		{
			name: "missing protocol", wantError: "did not select RDP",
			handle: func(stream grpc.BidiStreamingServer[pb.DisplayInput, pb.DisplayOutput]) error {
				return stream.Send(&pb.DisplayOutput{Payload: &pb.DisplayOutput_Data{Data: []byte("not metadata")}})
			},
		},
		{
			name: "unknown protocol", wantError: "did not select RDP",
			handle: func(stream grpc.BidiStreamingServer[pb.DisplayInput, pb.DisplayOutput]) error {
				return stream.Send(&pb.DisplayOutput{Payload: &pb.DisplayOutput_Ready{Ready: &pb.DisplayReady{Protocol: pb.DisplayProtocol(99)}}})
			},
		},
		{
			name: "repeated metadata", wantError: "unexpected display metadata",
			handle: func(stream grpc.BidiStreamingServer[pb.DisplayInput, pb.DisplayOutput]) error {
				ready := &pb.DisplayOutput{Payload: &pb.DisplayOutput_Ready{Ready: &pb.DisplayReady{Protocol: pb.DisplayProtocol_DISPLAY_PROTOCOL_RDP}}}
				if err := stream.Send(ready); err != nil {
					return err
				}
				return stream.Send(ready)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener := bufconn.Listen(1024 * 1024)
			rpc := grpc.NewServer()
			pb.RegisterSandboxServiceServer(rpc, displayFixture{handle: test.handle})
			go func() { _ = rpc.Serve(listener) }()
			defer rpc.Stop()
			defer listener.Close()
			client, err := grpc.NewClient("passthrough:///display-test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			backend, viewer := net.Pipe()
			defer viewer.Close()
			defer backend.Close()
			_ = viewer.SetDeadline(time.Now().Add(10 * time.Second))
			result := make(chan error, 1)
			go func() { result <- forwardDisplay(ctx, pb.NewSandboxServiceClient(client), "test-sandbox", backend) }()
			if test.echo {
				written := make(chan error, 1)
				go func() { _, err := viewer.Write(data); written <- err }()
				got := make([]byte, len(data))
				if _, err := io.ReadFull(viewer, got); err != nil {
					t.Fatal(err)
				}
				if err := <-written; err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, data) {
					t.Fatal("binary payload changed in transit")
				}
			}
			select {
			case err := <-result:
				if test.wantError == "" {
					if err != nil {
						t.Fatal(err)
					}
				} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("got %v, want error containing %q", err, test.wantError)
				}
			case <-ctx.Done():
				t.Fatal("display forwarding did not finish")
			}
		})
	}
}
