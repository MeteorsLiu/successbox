package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	pb "github.com/MeteorsLiu/successbox/api/sandbox/v1"
	"google.golang.org/grpc"
)

func main() {
	socket := flag.String("socket", "", "path to the private Unix socket")
	state := flag.String("state", "", "directory for this service's sandbox state")
	binary := flag.String("runsc", "runsc", "runsc executable")
	worktrees := flag.String("worktrees", "", "worktree policy executable")
	rdpPort := flag.Uint("rdp-port", 3389, "RDP port inside desktop sandboxes")
	flag.Parse()
	if runtime.GOOS != "linux" {
		log.Fatal("the gVisor backend runs on Linux; use successbox-apple on macOS")
	}
	if *socket == "" || *state == "" || *rdpPort == 0 || *rdpPort > 65535 {
		log.Fatal("--socket, --state and a valid --rdp-port are required")
	}
	if *worktrees != "" {
		if err := worktreeNamespace(); err != nil {
			log.Fatal(err)
		}
	}
	path, err := exec.LookPath(*binary)
	if err != nil {
		log.Fatal(err)
	}
	root, err := filepath.Abs(*state)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(*socket), 0700); err != nil {
		log.Fatal(err)
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(*socket, 0600); err != nil {
		log.Fatal(err)
	}
	if *worktrees != "" {
		if os.Geteuid() != 0 {
			log.Fatal("worktrees require a rootful backend")
		}
		*worktrees, err = exec.LookPath(*worktrees)
		if err != nil {
			log.Fatal(err)
		}
		// Private per-sandbox directories remain 0700; workers running as the
		// guest UID only need traversal to their own retained result directory.
		if err := os.Chmod(root, 0711); err != nil {
			log.Fatal(err)
		}
	}
	backend := &server{worktreeHelper: *worktrees, binary: path, root: root, rdpPort: uint16(*rdpPort), boxes: make(map[string]*sandbox)}
	rpc := grpc.NewServer()
	pb.RegisterSandboxServiceServer(rpc, backend)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-signals
		backend.shutdown()
		rpc.Stop()
	}()
	fmt.Fprintf(os.Stderr, "gVisor backend listening on %s\n", *socket)
	if err := rpc.Serve(listener); err != nil {
		log.Fatal(err)
	}
}
