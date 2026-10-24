package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/MeteorsLiu/successbox/api/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestDisplayRelay(t *testing.T) {
	for _, test := range []struct {
		name string
		port uint16
		code codes.Code
	}{
		{"binary and half close", 3300, codes.OK},
		{"disconnect", 3301, codes.Canceled},
		{"startup failure", 3302, codes.Unavailable},
		{"disconnect before guest pid", 3303, codes.Canceled},
		{"repeated connect", 3304, codes.InvalidArgument},
		{"sandbox close during display", 3305, codes.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			boxDir := filepath.Join(root, "box")
			runtimeDir := filepath.Join(root, "runsc")
			for _, dir := range []string{boxDir, runtimeDir} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			binary := filepath.Join(root, "runsc-test")
			// Model runsc's separate startup/PID publication and kill commands.
			// sleep deliberately ignores stdin EOF so cancellation must signal it.
			script := `#!/bin/sh
set -eu
test "$1" = --root
root=$2
shift 2
case "$1" in
exec)
  shift
  test "$1" = --internal-pid-file
  pid_file=$2
  shift 2
  test "$1" = test-sandbox
  test "$2" = /usr/bin/socat
  test "$3" = STDIO
  target=$4
  test "$target" != TCP:127.0.0.1:3302 || exit 127
  if test "$target" = TCP:127.0.0.1:3303; then
    printf '%s\n' "$$" > "$root/starting"
    while ! test -f "$root/release"; do sleep 0.01; done
  fi
  printf '%s\n' "$$" > "$pid_file.tmp"
  mv "$pid_file.tmp" "$pid_file"
  if test "$target" = TCP:127.0.0.1:3300; then
    printf 'relay diagnostics must not become RDP bytes\n' >&2
    exec /bin/cat
  fi
  exec /bin/sleep 60
  ;;
kill)
  shift
  if test "$1" = --all; then exit 0; fi
  test "$1" = --pid
  test "$3" = test-sandbox
  test "$4" = SIGKILL
  printf '%s\n' "$2" > "$root/killed"
  kill -KILL "$2"
  ;;
delete) exit 0 ;;
*) exit 2 ;;
esac
`
			if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			boxContext, closeBox := context.WithCancel(context.Background())
			box := &sandbox{id: "test-sandbox", dir: boxDir, options: &pb.CreateRequest{Desktop: true}, ctx: boxContext, cancel: closeBox}
			backend := &server{binary: binary, root: root, rdpPort: test.port, boxes: map[string]*sandbox{box.id: box}}
			listener := bufconn.Listen(1024 * 1024)
			rpc := grpc.NewServer()
			pb.RegisterSandboxServiceServer(rpc, backend)
			go func() { _ = rpc.Serve(listener) }()
			defer func() { closeBox(); rpc.Stop(); _ = listener.Close() }()
			connection, err := grpc.NewClient("passthrough:///relay", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			ctx, disconnect := context.WithTimeout(context.Background(), 10*time.Second)
			defer disconnect()
			stream, err := pb.NewSandboxServiceClient(connection).Display(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err := stream.Send(&pb.DisplayInput{Payload: &pb.DisplayInput_Connect{Connect: &pb.SandboxRef{Id: box.id}}}); err != nil {
				t.Fatal(err)
			}
			if test.port == 3303 {
				for {
					if _, err := os.Stat(filepath.Join(runtimeDir, "starting")); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						t.Fatal("runtime never started")
					case <-time.After(10 * time.Millisecond):
					}
				}
				disconnect()
				if err := os.WriteFile(filepath.Join(runtimeDir, "release"), nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			first, err := stream.Recv()
			if test.port == 3302 || test.port == 3303 {
				if status.Code(err) != test.code {
					t.Fatalf("startup: got %v, want %v", err, test.code)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if first.GetReady().GetProtocol() != pb.DisplayProtocol_DISPLAY_PROTOCOL_RDP {
					t.Fatal("missing RDP metadata")
				}
				switch test.port {
				case 3300:
					payload := bytes.Repeat([]byte{0, 1, 128, 255, 13, 10, 42, 0}, 65536)
					sent := make(chan error, 1)
					go func() {
						for offset := 0; offset < len(payload); offset += 16384 {
							if err := stream.Send(&pb.DisplayInput{Payload: &pb.DisplayInput_Data{Data: payload[offset : offset+16384]}}); err != nil {
								sent <- err
								return
							}
						}
						sent <- stream.CloseSend()
					}()
					var got []byte
					for {
						frame, err := stream.Recv()
						if err == io.EOF {
							break
						}
						if err != nil {
							t.Fatal(err)
						}
						if _, ok := frame.Payload.(*pb.DisplayOutput_Data); !ok {
							t.Fatal("metadata appeared inside RDP stream")
						}
						got = append(got, frame.GetData()...)
					}
					if err := <-sent; err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(got, payload) {
						t.Fatalf("binary relay changed data: got %d, want %d bytes", len(got), len(payload))
					}
				case 3301:
					disconnect()
					_, err = stream.Recv()
					if status.Code(err) != test.code {
						t.Fatalf("disconnect: %v", err)
					}
				case 3304:
					if err := stream.Send(&pb.DisplayInput{Payload: &pb.DisplayInput_Connect{Connect: &pb.SandboxRef{Id: box.id}}}); err != nil {
						t.Fatal(err)
					}
					_, err = stream.Recv()
					if status.Code(err) != test.code {
						t.Fatalf("repeated connect: %v", err)
					}
				case 3305:
					if _, err := pb.NewSandboxServiceClient(connection).Close(ctx, &pb.SandboxRef{Id: box.id}); err != nil {
						t.Fatal("sandbox close blocked on display:", err)
					}
					if _, err := stream.Recv(); err == nil || status.Code(err) == codes.DeadlineExceeded {
						t.Fatalf("display did not terminate after Close: %v", err)
					}
				}
			}
			cleaned := make(chan struct{})
			go func() { box.active.Wait(); close(cleaned) }()
			select {
			case <-cleaned:
			case <-time.After(5 * time.Second):
				t.Fatal("display process cleanup did not finish")
			}
			if test.port == 3301 || test.port == 3303 || test.port == 3304 {
				if _, err := os.Stat(filepath.Join(runtimeDir, "killed")); err != nil {
					t.Fatal("guest relay was not signaled:", err)
				}
			}
			entries, err := os.ReadDir(boxDir)
			if test.port == 3305 {
				if !os.IsNotExist(err) || box.ctx.Err() == nil {
					t.Fatalf("sandbox was not fully closed: %v", err)
				}
				return
			}
			if err != nil || len(entries) != 0 {
				t.Fatalf("display state was not removed: %v, %v", entries, err)
			}
			if box.ctx.Err() != nil {
				t.Fatal("display cleanup canceled the sandbox")
			}
		})
	}
}
