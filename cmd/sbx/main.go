package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"

	pb "github.com/MeteorsLiu/successbox/api/sandbox/v1"
	"github.com/containerd/console"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protojson"
)

func main() { os.Exit(run()) }

func run() int {
	flags := flag.NewFlagSet("sbx", flag.ContinueOnError)
	socket := flags.String("socket", "", "backend Unix socket")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	args := flags.Args()
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: sbx --socket PATH create FILE.json | exec [options] COMMAND... | display [options] | close ID; sbx gateways [options]")
		return 2
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if args[0] == "gateways" {
		if err := gateways(ctx, args[1:], os.Stderr); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	}
	if *socket == "" {
		fmt.Fprintln(os.Stderr, "--socket is required for sandbox commands")
		return 2
	}
	connection, err := grpc.NewClient("unix://"+*socket, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer connection.Close()
	client := pb.NewSandboxServiceClient(connection)
	code := 0
	switch args[0] {
	case "create":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "create requires one JSON file")
			return 2
		}
		var data []byte
		data, err = os.ReadFile(args[1])
		if err == nil {
			request := new(pb.CreateRequest)
			err = protojson.Unmarshal(data, request)
			if err == nil {
				var ref *pb.SandboxRef
				ref, err = client.Create(ctx, request)
				if err == nil {
					fmt.Println(ref.Id)
				}
			}
		}
	case "exec":
		code, err = execute(ctx, client, args[1:])
	case "display":
		err = display(ctx, client, args[1:])
	case "close":
		if len(args) != 2 {
			fmt.Fprintln(os.Stderr, "close requires a sandbox ID")
			return 2
		}
		_, err = client.Close(ctx, &pb.SandboxRef{Id: args[1]})
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return code
}

func execute(ctx context.Context, client pb.SandboxServiceClient, args []string) (int, error) {
	flags := flag.NewFlagSet("exec", flag.ContinueOnError)
	id := flags.String("sandbox", "", "sandbox ID")
	useTTY := flags.Bool("tty", false, "allocate a PTY")
	if err := flags.Parse(args); err != nil {
		return 2, err
	}
	if *id == "" || len(flags.Args()) == 0 {
		return 2, errors.New("exec requires --sandbox ID and a command")
	}
	stream, err := client.Exec(ctx)
	if err != nil {
		return 1, err
	}
	start := &pb.ExecStart{SandboxId: *id, Argv: flags.Args()}
	var terminal console.Console
	if *useTTY {
		terminal, err = console.ConsoleFromFile(os.Stdin)
		if err != nil {
			return 1, err
		}
		size, err := terminal.Size()
		if err != nil {
			return 1, err
		}
		start.Terminal = &pb.TerminalSize{Columns: uint32(size.Width), Rows: uint32(size.Height)}
		if err := terminal.SetRaw(); err != nil {
			return 1, err
		}
		defer terminal.Reset()
	}
	if err := stream.Send(&pb.ExecInput{Payload: &pb.ExecInput_Start{Start: start}}); err != nil {
		return 1, err
	}
	var sendMu sync.Mutex
	send := func(input *pb.ExecInput) error { sendMu.Lock(); defer sendMu.Unlock(); return stream.Send(input) }
	done := make(chan struct{})
	defer close(done)
	if terminal != nil {
		resize := make(chan os.Signal, 1)
		signal.Notify(resize, syscall.SIGWINCH)
		defer signal.Stop(resize)
		go func() {
			for {
				select {
				case <-done:
					return
				case <-resize:
					if size, err := terminal.Size(); err == nil {
						if send(&pb.ExecInput{Payload: &pb.ExecInput_Resize{Resize: &pb.TerminalSize{Columns: uint32(size.Width), Rows: uint32(size.Height)}}}) != nil {
							return
						}
					}
				}
			}
		}()
	}
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			n, err := os.Stdin.Read(buffer)
			if n > 0 {
				if send(&pb.ExecInput{Payload: &pb.ExecInput_Stdin{Stdin: append([]byte(nil), buffer[:n]...)}}) != nil {
					return
				}
			}
			if err != nil {
				if terminal == nil {
					_ = send(&pb.ExecInput{Payload: &pb.ExecInput_StdinClosed{StdinClosed: &pb.Empty{}}})
				}
				return
			}
		}
	}()
	for {
		message, err := stream.Recv()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return 1, errors.New("execution ended without an exit result")
			}
			return 1, err
		}
		switch data := message.Payload.(type) {
		case *pb.ExecOutput_Stdout:
			if _, err := os.Stdout.Write(data.Stdout); err != nil {
				return 1, err
			}
		case *pb.ExecOutput_Stderr:
			if _, err := os.Stderr.Write(data.Stderr); err != nil {
				return 1, err
			}
		case *pb.ExecOutput_Result:
			return int(data.Result.ExitCode), nil
		}
	}
}

func display(ctx context.Context, client pb.SandboxServiceClient, args []string) error {
	flags := flag.NewFlagSet("display", flag.ContinueOnError)
	id := flags.String("sandbox", "", "sandbox ID")
	address := flags.String("listen", "127.0.0.1:0", "loopback TCP address for a viewer")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("display requires --sandbox ID")
	}
	host, _, err := net.SplitHostPort(*address)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return errors.New("display proxy must listen on a literal loopback address")
	}
	listener, err := net.Listen("tcp", *address)
	if err != nil {
		return err
	}
	defer listener.Close()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	fmt.Fprintln(os.Stderr, "display byte-stream proxy:", listener.Addr())
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func() {
			defer connection.Close()
			if err := forwardDisplay(ctx, client, *id, connection); err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "display connection:", err)
			}
		}()
	}
}

func forwardDisplay(ctx context.Context, client pb.SandboxServiceClient, id string, connection net.Conn) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stop()
	stream, err := client.Display(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&pb.DisplayInput{Payload: &pb.DisplayInput_Connect{Connect: &pb.SandboxRef{Id: id}}}); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	ready := first.GetReady()
	if ready == nil || ready.Protocol != pb.DisplayProtocol_DISPLAY_PROTOCOL_RDP {
		return errors.New("display did not select RDP")
	}
	fmt.Fprintln(os.Stderr, "display protocol:", ready.Protocol)
	go func() {
		buffer := make([]byte, 32*1024)
		for {
			n, err := connection.Read(buffer)
			if n > 0 {
				if err := stream.Send(&pb.DisplayInput{Payload: &pb.DisplayInput_Data{Data: append([]byte(nil), buffer[:n]...)}}); err != nil {
					cancel()
					return
				}
			}
			if err != nil {
				_ = stream.CloseSend()
				if !errors.Is(err, io.EOF) {
					cancel()
				}
				return
			}
		}
	}()
	for {
		frame, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		data, ok := frame.Payload.(*pb.DisplayOutput_Data)
		if !ok {
			return errors.New("unexpected display metadata after stream start")
		}
		if _, err := connection.Write(data.Data); err != nil {
			return err
		}
	}
}
