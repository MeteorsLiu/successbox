package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "github.com/MeteorsLiu/successbox/api/sandbox/v1"
	"github.com/containerd/console"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type server struct {
	pb.UnimplementedSandboxServiceServer
	binary         string
	worktreeHelper string
	root           string
	rdpPort        uint16
	network        string
	slirp          string
	mu             sync.Mutex
	boxes          map[string]*sandbox
}

type sandbox struct {
	id, dir   string
	options   *pb.CreateRequest
	ctx       context.Context
	cancel    context.CancelFunc
	active    sync.WaitGroup
	closing   bool
	failed    bool
	closed    chan struct{}
	closeErr  error
	worktrees *exec.Cmd
	unmount   func() error
	net       *sandboxNetwork
}

func (s *server) command(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, s.binary, append([]string{"--root", filepath.Join(s.root, "runsc")}, args...)...)
}

func (s *server) acquire(id string) (*sandbox, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[id]
	if b == nil {
		return nil, status.Error(codes.NotFound, "sandbox does not exist")
	}
	if b.closing || b.failed {
		return nil, status.Error(codes.FailedPrecondition, "sandbox is closing or cleanup failed")
	}
	b.active.Add(1)
	return b, nil
}

func (s *server) Create(ctx context.Context, request *pb.CreateRequest) (*pb.SandboxRef, error) {
	if request.GetRootfs() == "" || !filepath.IsAbs(request.Rootfs) {
		return nil, status.Error(codes.InvalidArgument, "rootfs must be an absolute directory path")
	}
	fi, err := os.Stat(request.Rootfs)
	if err != nil || !fi.IsDir() {
		return nil, status.Error(codes.InvalidArgument, "rootfs is not an accessible directory")
	}
	if os.Geteuid() != 0 && (request.Cpus != 0 || request.MemoryBytes != 0) {
		return nil, status.Error(codes.Unimplemented, "resource limits require a cgroup-capable privileged backend")
	}
	for _, value := range request.Env {
		key, _, ok := strings.Cut(value, "=")
		if !ok || key == "" || strings.ContainsRune(value, 0) {
			return nil, status.Error(codes.InvalidArgument, "invalid environment entry")
		}
	}
	for _, mount := range request.Mounts {
		if !filepath.IsAbs(mount.Source) || !filepath.IsAbs(mount.Destination) || filepath.Clean(mount.Destination) == "/" {
			return nil, status.Error(codes.InvalidArgument, "mount paths must be absolute and destination must not replace rootfs")
		}
		if _, err := os.Stat(mount.Source); err != nil {
			return nil, status.Error(codes.InvalidArgument, "mount source does not exist")
		}
	}
	if request.WorkingDir != "" && !filepath.IsAbs(request.WorkingDir) {
		return nil, status.Error(codes.InvalidArgument, "working_dir must be absolute")
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, status.Error(codes.Internal, "cannot allocate sandbox ID")
	}
	id := hex.EncodeToString(random[:])
	dir := filepath.Join(s.root, id)
	if err := os.Mkdir(dir, 0700); err != nil {
		return nil, status.Error(codes.Internal, "cannot create sandbox state")
	}
	created := false
	var worktrees *exec.Cmd
	var unmount func() error
	var network *sandboxNetwork
	var worktreeSocketDir string
	defer func() {
		if !created {
			if network != nil {
				network.cleanup()
			}
			_ = s.command(context.Background(), "delete", "--force", id).Run()
			if worktrees != nil {
				worktrees.Process.Signal(syscall.SIGTERM)
				_ = worktrees.Wait()
			}
			if unmount != nil {
				if err := unmount(); err != nil {
					return
				}
			}
			_ = os.RemoveAll(worktreeSocketDir)
			_ = os.RemoveAll(dir)
		}
	}()
	if s.network == "slirp4netns" {
		network, err = createSandboxNetwork(dir, s.slirp)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "prepare sandbox network: %v", err)
		}
	}
	filestore := filepath.Join(dir, "overlay")
	if err := os.Mkdir(filestore, 0700); err != nil {
		return nil, status.Error(codes.Internal, "cannot create overlay storage")
	}
	spec := makeSpec(request)
	if s.worktreeHelper != "" {
		// macOS profile paths can exceed sockaddr_un's 107-byte limit once
		// the sandbox ID is appended. Only the socket needs a short path.
		worktreeSocketDir, err = os.MkdirTemp("/tmp", "sbx-wt-")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "prepare worktree socket: %v", err)
		}
		if err := hideWorktreeState(spec, worktreeSocketDir); err != nil {
			return nil, status.Errorf(codes.Internal, "hide worktree socket: %v", err)
		}
		if err := hideWorktreeState(spec, s.root); err != nil {
			return nil, status.Errorf(codes.Internal, "hide runtime state: %v", err)
		}
	}
	if network != nil {
		resolv := filepath.Join(dir, "resolv.conf")
		if err := os.WriteFile(resolv, []byte("nameserver 10.0.2.3\n"), 0644); err != nil {
			return nil, status.Errorf(codes.Internal, "prepare sandbox DNS: %v", err)
		}
		spec.Mounts = append(spec.Mounts, specs.Mount{Source: resolv, Destination: "/etc/resolv.conf", Type: "bind", Options: []string{"rbind", "ro", "nosuid", "nodev"}})
		spec.Linux.Namespaces = append(spec.Linux.Namespaces, specs.LinuxNamespace{Type: specs.NetworkNamespace, Path: network.path})
	}
	if s.worktreeHelper != "" {
		results := filepath.Join(s.root, "results", id)
		// Credential workers open this per-sandbox parent with os.OpenRoot,
		// which needs read permission as well as traversal. Each private child
		// remains 0700 and belongs to the credentials that prepared it.
		if err := os.MkdirAll(results, 0755); err != nil {
			return nil, status.Errorf(codes.Internal, "prepare results: %v", err)
		}
		spec.Mounts = append(spec.Mounts, specs.Mount{Source: results, Destination: "/run/successbox-results", Type: "bind", Options: []string{"rbind", "rprivate", "nosuid", "nodev"}})
		policyDir := filepath.Join(dir, "worktrees")
		if err := os.Mkdir(policyDir, 0700); err != nil {
			return nil, status.Errorf(codes.Internal, "prepare worktree config: %v", err)
		}
		data, err := json.MarshalIndent(spec, "", "  ")
		if err != nil {
			return nil, status.Errorf(codes.Internal, "encode worktree config: %v", err)
		}
		if err := os.WriteFile(filepath.Join(policyDir, "config.json"), data, 0600); err != nil {
			return nil, status.Errorf(codes.Internal, "write worktree config: %v", err)
		}
		unmount, err = worktreeMounts(spec, dir)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "prepare private mounts: %v", err)
		}
		unmountViews := unmount
		unmount = func() error {
			if err := unmountViews(); err != nil {
				return err
			}
			return os.RemoveAll(worktreeSocketDir)
		}
	}

	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, status.Error(codes.Internal, "cannot encode OCI spec")
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0600); err != nil {
		return nil, status.Error(codes.Internal, "cannot write OCI spec")
	}
	runscNetwork := "none"
	if network != nil {
		runscNetwork = "sandbox"
	}
	args := []string{"--network=" + runscNetwork, "--overlay2=all:dir=" + filestore}
	if s.worktreeHelper != "" {
		args[1] = "--overlay2=root:dir=" + filestore
		args = append(args, "--platform=successbox", "--platform_device_path="+filepath.Join(worktreeSocketDir, "control.sock"))
	}
	if os.Geteuid() != 0 {
		args = append(args, "--rootless=true", "--ignore-cgroups")
	}
	if request.Desktop {
		args = append(args, "--in-sandbox-cgroup=v2", "--allow-suid")
	}
	args = append(args, "run", "--detach", "--bundle", dir, id)
	logFile, err := os.OpenFile(filepath.Join(dir, "runtime.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, status.Error(codes.Internal, "cannot create runtime log")
	}
	defer logFile.Close()
	if s.worktreeHelper != "" {
		worktrees, err = s.startWorktrees(ctx, id, dir, filepath.Join(worktreeSocketDir, "control.sock"), logFile)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "worktree startup failed: %v", err)
		}
	}
	cmd := s.command(ctx, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Run(); err != nil {
		return nil, status.Errorf(codes.Internal, "runsc startup failed: %v", err)
	}
	boxCtx, cancel := context.WithCancel(context.Background())
	b := &sandbox{unmount: unmount, worktrees: worktrees, net: network, id: id, dir: dir, options: proto.Clone(request).(*pb.CreateRequest), ctx: boxCtx, cancel: cancel}
	s.mu.Lock()
	s.boxes[id] = b
	s.mu.Unlock()
	created = true
	return &pb.SandboxRef{Id: id}, nil
}

func makeSpec(r *pb.CreateRequest) *specs.Spec {
	argv := r.InitArgv
	if len(argv) == 0 {
		argv = []string{"/bin/sleep", "infinity"}
	}
	cwd := r.WorkingDir
	if cwd == "" {
		cwd = "/"
	}
	user := specs.User{}
	if r.User != nil {
		user.UID, user.GID = r.User.Uid, r.User.Gid
	}
	env := append([]string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}, r.Env...)
	capabilities := []string{"CAP_CHOWN", "CAP_DAC_OVERRIDE", "CAP_FOWNER", "CAP_FSETID", "CAP_KILL", "CAP_SETGID", "CAP_SETUID", "CAP_SETPCAP", "CAP_NET_BIND_SERVICE", "CAP_SYS_CHROOT", "CAP_MKNOD", "CAP_AUDIT_WRITE", "CAP_SETFCAP"}
	if r.Desktop {
		capabilities = append(capabilities, "CAP_SYS_ADMIN", "CAP_SYS_RESOURCE", "CAP_SYS_BOOT")
	}
	spec := &specs.Spec{
		Version:  specs.Version,
		Root:     &specs.Root{Path: r.Rootfs},
		Hostname: r.Hostname,
		Process:  &specs.Process{Args: argv, Env: env, Cwd: cwd, User: user, Capabilities: &specs.LinuxCapabilities{Bounding: capabilities, Effective: capabilities, Permitted: capabilities}},
		Linux:    &specs.Linux{Namespaces: []specs.LinuxNamespace{{Type: specs.PIDNamespace}, {Type: specs.MountNamespace}, {Type: specs.IPCNamespace}, {Type: specs.UTSNamespace}}},
		Mounts: []specs.Mount{
			{Destination: "/proc", Type: "proc", Source: "proc"},
			{Destination: "/dev", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "mode=755"}},
			{Destination: "/dev/pts", Type: "devpts", Source: "devpts", Options: []string{"nosuid", "noexec", "newinstance", "ptmxmode=0666", "mode=0620"}},
			{Destination: "/dev/shm", Type: "tmpfs", Source: "shm", Options: []string{"nosuid", "nodev", "mode=1777"}},
			{Destination: "/sys", Type: "sysfs", Source: "sysfs", Options: []string{"nosuid", "noexec", "nodev", "ro"}},
			{Destination: "/sys/fs/cgroup", Type: "cgroup", Source: "cgroup", Options: []string{"nosuid", "noexec", "nodev"}},
			{Destination: "/run", Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "mode=755"}},
		},
	}
	if os.Geteuid() != 0 {
		spec.Linux.Namespaces = append(spec.Linux.Namespaces, specs.LinuxNamespace{Type: specs.UserNamespace})
		spec.Linux.UIDMappings = []specs.LinuxIDMapping{{ContainerID: 0, HostID: uint32(os.Getuid()), Size: 1}}
		spec.Linux.GIDMappings = []specs.LinuxIDMapping{{ContainerID: 0, HostID: uint32(os.Getgid()), Size: 1}}
	}
	if r.MemoryBytes != 0 || r.Cpus != 0 {
		spec.Linux.Resources = &specs.LinuxResources{}
		if r.MemoryBytes != 0 {
			limit := int64(r.MemoryBytes)
			spec.Linux.Resources.Memory = &specs.LinuxMemory{Limit: &limit}
		}
		if r.Cpus != 0 {
			period, quota := uint64(100000), int64(r.Cpus)*100000
			spec.Linux.Resources.CPU = &specs.LinuxCPU{Period: &period, Quota: &quota}
		}
	}
	for _, m := range r.Mounts {
		options := []string{"rbind", "rprivate", "nosuid", "nodev"}
		if m.ReadOnly {
			options = append(options, "ro")
		}
		spec.Mounts = append(spec.Mounts, specs.Mount{Source: m.Source, Destination: filepath.Clean(m.Destination), Type: "bind", Options: options})
	}
	return spec
}

type outputWriter struct {
	stream grpc.BidiStreamingServer[pb.ExecInput, pb.ExecOutput]
	mu     *sync.Mutex
	stderr bool
}

func (w outputWriter) Write(p []byte) (int, error) {
	// The runtime owns the execution. After its RPC disconnects we still
	// drain the pipe, so a child cannot block forever on an abandoned stdout.
	if w.stream.Context().Err() != nil {
		return len(p), nil
	}
	message := &pb.ExecOutput{Payload: &pb.ExecOutput_Stdout{Stdout: append([]byte(nil), p...)}}
	if w.stderr {
		message.Payload = &pb.ExecOutput_Stderr{Stderr: append([]byte(nil), p...)}
	}
	w.mu.Lock()
	err := w.stream.Send(message)
	w.mu.Unlock()
	_ = err // Transport failure must not turn into a broken stdout for the guest.
	return len(p), nil
}

func (s *server) Exec(stream grpc.BidiStreamingServer[pb.ExecInput, pb.ExecOutput]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil || len(start.Argv) == 0 {
		return status.Error(codes.InvalidArgument, "first message must start a non-empty argv")
	}
	b, err := s.acquire(start.SandboxId)
	if err != nil {
		return err
	}
	owned := false
	defer func() {
		if !owned {
			b.active.Done()
		}
	}()
	work, err := os.MkdirTemp(b.dir, "exec-")
	if err != nil {
		return status.Error(codes.Internal, "cannot create execution state")
	}
	pidPath := filepath.Join(work, "pid")
	args := []string{"exec", "--internal-pid-file", pidPath}
	var tty console.Console
	var slave *os.File
	var stdin io.WriteCloser
	var stdinReader *os.File
	if start.Terminal != nil {
		if start.Terminal.Rows == 0 || start.Terminal.Columns == 0 || start.Terminal.Rows > 65535 || start.Terminal.Columns > 65535 {
			_ = os.RemoveAll(work)
			return status.Error(codes.InvalidArgument, "invalid terminal size")
		}
		var slavePath string
		tty, slavePath, err = console.NewPty()
		if err != nil {
			return status.Error(codes.Internal, "cannot create PTY")
		}
		slave, err = os.OpenFile(slavePath, os.O_RDWR, 0)
		if err != nil {
			_ = tty.Close()
			return status.Error(codes.Internal, "cannot open PTY slave")
		}
		if err := tty.Resize(console.WinSize{Width: uint16(start.Terminal.Columns), Height: uint16(start.Terminal.Rows)}); err != nil {
			_ = slave.Close()
			_ = tty.Close()
			return status.Error(codes.Internal, "cannot size PTY")
		}
		stdin = tty
	} else {
		var writer *os.File
		stdinReader, writer, err = os.Pipe()
		if err != nil {
			return status.Error(codes.Internal, "cannot create stdin pipe")
		}
		stdin = writer
	}
	args = append(args, b.id)
	args = append(args, start.Argv...)
	cmd := s.command(b.ctx, args...)
	var sendMu sync.Mutex
	out := outputWriter{stream: stream, mu: &sendMu}
	if tty != nil {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	} else {
		cmd.Stdin = stdinReader
		cmd.Stdout, cmd.Stderr = out, outputWriter{stream: stream, mu: &sendMu, stderr: true}
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		if stdinReader != nil {
			_ = stdinReader.Close()
		}
		if slave != nil {
			_ = slave.Close()
			_ = tty.Close()
		}
		_ = os.RemoveAll(work)
		return status.Error(codes.Internal, "cannot start runsc exec")
	}
	owned = true
	if stdinReader != nil {
		_ = stdinReader.Close()
	}
	if slave != nil {
		_ = slave.Close()
	}
	done := make(chan int32, 1)
	processDone := make(chan struct{})
	var drain sync.WaitGroup
	if tty != nil {
		drain.Add(1)
		go func() { defer drain.Done(); _, _ = io.Copy(out, tty) }()
	}
	go func() {
		defer b.active.Done()
		defer os.RemoveAll(work)
		err := cmd.Wait()
		close(processDone)
		var code int32
		if err != nil {
			code = 1
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = int32(exit.ExitCode())
				if state, ok := exit.Sys().(syscall.WaitStatus); ok && state.Signaled() {
					code = int32(128 + state.Signal())
				}
			}
		}
		if tty != nil {
			drain.Wait()
			_ = tty.Close()
		} else {
			_ = stdin.Close()
		}
		done <- code
	}()
	inputErrors := make(chan error, 1)
	go func() {
		closed := false
		for {
			message, err := stream.Recv()
			if err != nil {
				if tty == nil {
					_ = stdin.Close()
				}
				if err != io.EOF {
					inputErrors <- err
				}
				return
			}
			switch payload := message.Payload.(type) {
			case *pb.ExecInput_Stdin:
				if closed {
					inputErrors <- status.Error(codes.InvalidArgument, "stdin is already closed")
					return
				}
				if _, err := stdin.Write(payload.Stdin); err != nil {
					return
				}
			case *pb.ExecInput_StdinClosed:
				if tty != nil {
					inputErrors <- status.Error(codes.FailedPrecondition, "PTY input uses terminal control characters, not stdin_closed")
					return
				}
				closed = true
				_ = stdin.Close()
			case *pb.ExecInput_Resize:
				if tty == nil || payload.Resize.Rows == 0 || payload.Resize.Columns == 0 || payload.Resize.Rows > 65535 || payload.Resize.Columns > 65535 {
					inputErrors <- status.Error(codes.InvalidArgument, "resize requires an active PTY and valid dimensions")
					return
				}
				if err := tty.Resize(console.WinSize{Width: uint16(payload.Resize.Columns), Height: uint16(payload.Resize.Rows)}); err != nil {
					inputErrors <- status.Error(codes.Internal, "PTY resize failed")
					return
				}
			case *pb.ExecInput_Signal:
				if payload.Signal < 1 || payload.Signal > 64 {
					inputErrors <- status.Error(codes.InvalidArgument, "invalid Linux signal")
					return
				}
				for {
					data, err := os.ReadFile(pidPath)
					if err == nil {
						pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
						if err != nil {
							inputErrors <- status.Error(codes.Internal, "invalid runtime process ID")
							return
						}
						if err := s.command(b.ctx, "kill", "--pid", strconv.Itoa(pid), b.id, strconv.Itoa(int(payload.Signal))).Run(); err != nil {
							inputErrors <- status.Error(codes.Internal, "signal delivery failed")
							return
						}
						break
					}
					if !os.IsNotExist(err) {
						inputErrors <- status.Error(codes.Internal, "cannot read runtime process ID")
						return
					}
					select {
					case <-processDone:
						return
					case <-stream.Context().Done():
						return
					case <-time.After(10 * time.Millisecond):
					}
				}
			default:
				inputErrors <- status.Error(codes.InvalidArgument, "start may only appear once")
				return
			}
		}
	}()
	select {
	case code := <-done:
		sendMu.Lock()
		defer sendMu.Unlock()
		return stream.Send(&pb.ExecOutput{Payload: &pb.ExecOutput_Result{Result: &pb.ExecResult{ExitCode: code}}})
	case err := <-inputErrors:
		return err
	case <-stream.Context().Done():
		return status.FromContextError(stream.Context().Err()).Err()
	}
}

func (s *server) Display(stream grpc.BidiStreamingServer[pb.DisplayInput, pb.DisplayOutput]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetConnect() == nil {
		return status.Error(codes.InvalidArgument, "first display message must select a sandbox")
	}
	b, err := s.acquire(first.GetConnect().Id)
	if err != nil {
		return err
	}
	defer b.active.Done()
	if !b.options.Desktop {
		return status.Error(codes.FailedPrecondition, "sandbox has no desktop")
	}
	dir, err := os.MkdirTemp(b.dir, "display-")
	if err != nil {
		return status.Error(codes.Internal, "cannot prepare display transport")
	}
	defer os.RemoveAll(dir)
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		return status.Error(codes.Internal, "cannot create display input")
	}
	defer stdinReader.Close()
	defer stdinWriter.Close()
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return status.Error(codes.Internal, "cannot create display output")
	}
	defer stdoutReader.Close()
	defer stdoutWriter.Close()
	pidPath := filepath.Join(dir, "pid")
	ctx, cancel := context.WithCancel(b.ctx)
	defer cancel()
	cmd := s.command(ctx, "exec", "--internal-pid-file", pidPath, b.id, "/usr/bin/socat", "STDIO", "TCP:127.0.0.1:"+strconv.Itoa(int(s.rdpPort)))
	cmd.Stdin, cmd.Stdout = stdinReader, stdoutWriter
	// stderr remains separate from the RDP wire stream. A failed relay is
	// reported through the final RPC status, never as display bytes.
	if err := cmd.Start(); err != nil {
		return status.Error(codes.Internal, "cannot start display transport")
	}
	_ = stdinReader.Close()
	_ = stdoutWriter.Close()
	finished := make(chan struct{})
	var processErr error // Read only after finished closes.
	go func() {
		processErr = cmd.Wait()
		close(finished)
	}()
	pid := 0
	defer func() {
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
		if pid != 0 {
			select {
			case <-finished:
			default:
				_ = s.command(b.ctx, "kill", "--pid", strconv.Itoa(pid), b.id, "SIGKILL").Run()
			}
		} else {
			cancel()
		}
		<-finished
	}()
	// Even an already-disconnected viewer must wait for the guest PID (or
	// startup failure). Killing only the runsc CLI can leave socat in the guest.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for pid == 0 {
		data, err := os.ReadFile(pidPath)
		if err == nil {
			value, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil || value <= 0 {
				return status.Error(codes.Internal, "invalid display process ID")
			}
			pid = value
			break
		}
		if !os.IsNotExist(err) {
			return status.Error(codes.Internal, "cannot read display process ID")
		}
		select {
		case <-finished:
			return status.Errorf(codes.Unavailable, "guest RDP relay did not start: %v", processErr)
		case <-ticker.C:
		}
	}
	stop := context.AfterFunc(stream.Context(), func() { _ = stdinWriter.Close(); _ = stdoutReader.Close() })
	defer stop()
	stopBox := context.AfterFunc(b.ctx, func() { _ = stdinWriter.Close(); _ = stdoutReader.Close() })
	defer stopBox()
	if err := stream.Send(&pb.DisplayOutput{Payload: &pb.DisplayOutput_Ready{Ready: &pb.DisplayReady{Protocol: pb.DisplayProtocol_DISPLAY_PROTOCOL_RDP}}}); err != nil {
		return err
	}
	inputErr := make(chan error, 1)
	go func() {
		for {
			frame, err := stream.Recv()
			if err == io.EOF {
				_ = stdinWriter.Close()
				return
			}
			if err != nil {
				inputErr <- err
				_ = stdinWriter.Close()
				_ = stdoutReader.Close()
				return
			}
			payload, ok := frame.Payload.(*pb.DisplayInput_Data)
			if !ok {
				inputErr <- status.Error(codes.InvalidArgument, "connect may only appear once")
				_ = stdinWriter.Close()
				_ = stdoutReader.Close()
				return
			}
			if _, err := stdinWriter.Write(payload.Data); err != nil {
				inputErr <- err
				_ = stdoutReader.Close()
				return
			}
		}
	}()
	buffer := make([]byte, 32*1024)
	for {
		n, err := stdoutReader.Read(buffer)
		if n != 0 {
			if err := stream.Send(&pb.DisplayOutput{Payload: &pb.DisplayOutput_Data{Data: append([]byte(nil), buffer[:n]...)}}); err != nil {
				return err
			}
		}
		if err != nil {
			select {
			case err := <-inputErr:
				return err
			default:
			}
			if stream.Context().Err() != nil {
				return status.FromContextError(stream.Context().Err()).Err()
			}
			if b.ctx.Err() != nil {
				return status.FromContextError(b.ctx.Err()).Err()
			}
			if !errors.Is(err, io.EOF) {
				return status.Error(codes.Unavailable, "display connection ended unexpectedly")
			}
			select {
			case <-finished:
				if processErr != nil {
					return status.Errorf(codes.Unavailable, "guest RDP relay failed: %v", processErr)
				}
				return nil
			case <-stream.Context().Done():
				return status.FromContextError(stream.Context().Err()).Err()
			case <-b.ctx.Done():
				return status.FromContextError(b.ctx.Err()).Err()
			}
		}
	}
}

func (s *server) Close(ctx context.Context, ref *pb.SandboxRef) (*pb.Empty, error) {
	if ref.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "sandbox ID is required")
	}
	s.mu.Lock()
	b := s.boxes[ref.Id]
	if b == nil {
		s.mu.Unlock()
		return &pb.Empty{}, nil
	}
	if b.closing {
		done := b.closed
		s.mu.Unlock()
		select {
		case <-done:
			return &pb.Empty{}, b.closeErr
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}
	b.closing, b.closed = true, make(chan struct{})
	s.mu.Unlock()
	// Finish cleanup independently of the caller's connection. A canceled
	// Close RPC must not abandon a half-destroyed sandbox.
	go func() {
		_ = s.command(context.Background(), "kill", "--all", b.id, "SIGKILL").Run()
		b.cancel()
		b.active.Wait()
		err := s.command(context.Background(), "delete", "--force", b.id).Run()
		if err == nil && b.worktrees != nil {
			err = b.worktrees.Process.Signal(syscall.SIGTERM)
			err = errors.Join(err, b.worktrees.Wait())
		}
		if err == nil && b.net != nil {
			b.net.cleanup()
		}
		if err == nil && b.unmount != nil {
			err = b.unmount()
		}
		if err == nil {
			err = os.RemoveAll(b.dir)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if err == nil {
			delete(s.boxes, b.id)
		} else {
			b.closeErr = status.Errorf(codes.Internal, "sandbox cleanup failed: %v", err)
			b.failed = true
		}
		close(b.closed)
	}()
	select {
	case <-b.closed:
		return &pb.Empty{}, b.closeErr
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

func (s *server) shutdown() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.boxes))
	for id := range s.boxes {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		_, _ = s.Close(context.Background(), &pb.SandboxRef{Id: id})
	}
}
