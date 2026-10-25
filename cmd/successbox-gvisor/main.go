package main

import (
	"flag"
	"fmt"
	"io"
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
	socketUID := flag.Int("socket-uid", -1, "owner UID of the private control socket; defaults to the service user")
	stdinLifetime := flag.Bool("exit-on-stdin-close", false, "stop the service when its launching desktop closes stdin")
	state := flag.String("state", "", "directory for this service's sandbox state")
	binary := flag.String("runsc", "runsc", "runsc executable")
	worktrees := flag.String("worktrees", "", "worktree policy executable")
	rdpPort := flag.Uint("rdp-port", 3389, "RDP port inside desktop sandboxes")
	network := flag.String("network", "none", "sandbox network: none or slirp4netns")
	slirp := flag.String("slirp4netns", "slirp4netns", "slirp4netns executable")
	flag.Parse()
	if runtime.GOOS != "linux" {
		log.Fatal("the gVisor backend runs on Linux; use successbox-apple on macOS")
	}
	if *socketUID < -1 || uint64(*socketUID+1) >= 1<<32 || (*socketUID != -1 && os.Geteuid() != 0) {
		log.Fatal("--socket-uid requires a rootful backend and a valid UID")
	}
	if *socket == "" || *state == "" || *rdpPort == 0 || *rdpPort > 65535 {
		log.Fatal("--socket, --state and a valid --rdp-port are required")
	}
	if *network != "none" && *network != "slirp4netns" {
		log.Fatal("--network must be none or slirp4netns")
	}
	if *network == "slirp4netns" && os.Geteuid() != 0 {
		log.Fatal("--network=slirp4netns requires a rootful backend")
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
	slirpPath := ""
	if *network == "slirp4netns" {
		slirpPath, err = exec.LookPath(*slirp)
		if err != nil {
			log.Fatal(err)
		}
	}
	listener, err := net.Listen("unix", *socket)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(*socket, 0600); err != nil {
		log.Fatal(err)
	}
	if *socketUID != -1 {
		if err := os.Chown(*socket, *socketUID, -1); err != nil {
			log.Fatal(err)
		}
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
	backend := &server{worktreeHelper: *worktrees, binary: path, root: root, rdpPort: uint16(*rdpPort), network: *network, slirp: slirpPath, boxes: make(map[string]*sandbox)}
	rpc := grpc.NewServer()
	pb.RegisterSandboxServiceServer(rpc, backend)
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	desktopClosed := make(chan struct{})
	if *stdinLifetime {
		go func() {
			_, _ = io.Copy(io.Discard, os.Stdin)
			close(desktopClosed)
		}()
	}
	go func() {
		select {
		case <-signals:
		case <-desktopClosed:
		}
		backend.shutdown()
		rpc.Stop()
	}()
	fmt.Fprintf(os.Stderr, "gVisor backend listening on %s\n", *socket)
	if err := rpc.Serve(listener); err != nil {
		log.Fatal(err)
	}
}
