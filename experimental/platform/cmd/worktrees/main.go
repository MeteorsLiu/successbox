//go:build linux

// worktrees prepares private repositories for the successbox platform. Its
// socket is a runtime device, never a guest FD or a guest-visible service.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/lisafs"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/runsc/fsgofer"
)

type request struct {
	Path     string
	UID, GID uint32
	Groups   []uint32
}
type response struct {
	Path  string
	Error string
}
type repository struct {
	Source, Directory   string
	UID, GID            uint32
	Groups              []uint32
	GitDevice, GitInode uint64
}
type manager struct {
	root, directory string
	mounts          []specs.Mount
	mu              sync.Mutex
	repositories    map[string]*repository
	stops           []func()
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "publish" {
		if err := publish(&repository{Source: os.Args[2], Directory: os.Args[3]}); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 4 && os.Args[1] == "prepare" {
		if err := prepare(os.Args[2], os.Args[3]); err != nil {
			log.Fatal(err)
		}
		return
	}
	socket := flag.String("socket", "", "private platform device socket")
	bundle := flag.String("bundle", "", "existing OCI bundle")
	directory := flag.String("state", "", "retained repository directory")
	readyFD := flag.Int("ready-fd", -1, "startup notification FD")
	flag.Parse()
	if *socket == "" || *bundle == "" || *directory == "" {
		log.Fatal("--socket, --bundle and --state are required")
	}
	// The first implementation preserves host inode owners directly. A rootless
	// gofer needs a matching user namespace before it can represent these IDs.
	if os.Geteuid() != 0 {
		log.Fatal("experimental worktrees currently require a rootful backend")
	}
	data, err := os.ReadFile(filepath.Join(*bundle, "config.json"))
	if err != nil {
		log.Fatal(err)
	}
	var spec specs.Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		log.Fatal(err)
	}
	if spec.Root == nil {
		log.Fatal("OCI root is missing")
	}
	root := spec.Root.Path
	if !filepath.IsAbs(root) {
		root = filepath.Join(*bundle, root)
	}
	for i := range spec.Mounts {
		if spec.Mounts[i].Type == "bind" {
			source, err := filepath.EvalSymlinks(spec.Mounts[i].Source)
			if err != nil {
				log.Fatal(err)
			}
			spec.Mounts[i].Source = source
		}
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(*directory, 0711); err != nil {
		log.Fatal(err)
	}
	if err := fsgofer.OpenProcSelfFD("/proc/self/fd"); err != nil {
		log.Fatal(err)
	}
	listener, err := unet.BindAndListen(*socket, true)
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()
	defer os.Remove(*socket)
	if err := os.Chmod(*socket, 0600); err != nil {
		log.Fatal(err)
	}
	if spec.Root.Readonly {
		root = ""
	}
	m := &manager{root: root, directory: *directory, mounts: spec.Mounts, repositories: make(map[string]*repository)}
	if *readyFD >= 0 {
		f := os.NewFile(uintptr(*readyFD), "ready")
		if _, err := f.Write([]byte{1}); err != nil {
			log.Fatal(err)
		}
		f.Close()
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	var clients sync.WaitGroup
	var peersMu sync.Mutex
	var peers []*unet.Socket
	stopped := make(chan struct{})
	go func() {
		<-signals
		listener.Close()
		peersMu.Lock()
		for _, p := range peers {
			p.Shutdown()
		}
		peersMu.Unlock()
		close(stopped)
	}()
	for {
		peer, err := listener.Accept()
		if err != nil {
			break
		}
		peersMu.Lock()
		peers = append(peers, peer)
		peersMu.Unlock()
		clients.Add(1)
		go func() {
			defer clients.Done()
			defer peer.Close()
			if err := m.serve(peer); err != nil && !errors.Is(err, io.EOF) {
				log.Print(err)
			}
		}()
	}
	<-stopped
	clients.Wait()
	for _, stop := range m.stops {
		stop()
	}
	failed := false
	for _, repo := range m.repositories {
		var metadata unix.Stat_t
		err := unix.Lstat(filepath.Join(repo.Source, ".git"), &metadata)
		if err == nil && (uint64(metadata.Dev) != repo.GitDevice || metadata.Ino != repo.GitInode) {
			err = errors.New("original repository identity changed")
		}
		exe, exeErr := os.Executable()
		err = errors.Join(err, exeErr)
		if err == nil {
			cmd := exec.Command(exe, "publish", repo.Source, repo.Directory)
			cmd.Env = gitEnvironment()
			cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: repo.UID, Gid: repo.GID, Groups: repo.Groups}}
			var output []byte
			output, err = cmd.CombinedOutput()
			if err != nil {
				err = fmt.Errorf("%w: %s", err, output)
			}
		}
		if err != nil {
			log.Printf("retained %s; worktree registration failed: %v", repo.Directory, err)
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
}

func (m *manager) serve(peer *unet.Socket) error {
	buffer := make([]byte, 64*1024)
	for {
		n, err := peer.Read(buffer)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.EOF
		}
		if n == len(buffer) {
			return errors.New("oversized worktree request")
		}
		var req request
		if err := json.Unmarshal(buffer[:n], &req); err != nil {
			return err
		}
		res, client, err := m.prepare(req)
		if err != nil {
			res.Error = err.Error()
		}
		data, err := json.Marshal(res)
		if err != nil {
			return err
		}
		writer := peer.Writer(true)
		if client >= 0 {
			writer.PackFDs(client)
		}
		n, err = writer.WriteVec([][]byte{data})
		if client >= 0 {
			unix.Close(client)
		}
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
	}
}

func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

func (m *manager) prepare(req request) (response, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !filepath.IsAbs(req.Path) || strings.ContainsRune(req.Path, 0) {
		return response{}, -1, errors.New("invalid guest path")
	}
	guest := filepath.Clean(req.Path)
	sourceRoot, guestRoot := m.root, "/"
	longest := 0
	for _, mount := range m.mounts {
		target := filepath.Clean(mount.Destination)
		if within(guest, target) && len(target) > longest {
			longest = len(target)
			if mount.Type != "bind" {
				sourceRoot = ""
			} else {
				sourceRoot = mount.Source
			}
			for _, option := range mount.Options {
				if option == "ro" {
					sourceRoot = ""
				}
			}
			guestRoot = target
		}
	}
	if sourceRoot == "" {
		return response{}, -1, nil
	}
	relative, _ := filepath.Rel(guestRoot, guest)
	source := filepath.Join(sourceRoot, relative)
	if within(source, m.directory) {
		return response{}, -1, nil
	}
	// Paths come from Sentry's permission-checked, symlink-resolved VFS lookup.
	// Never search above the root of the host mount that supplied that path.
	for {
		fi, err := os.Lstat(filepath.Join(source, ".git"))
		if err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			break
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOTDIR) {
			return response{}, -1, err
		}
		if source == sourceRoot {
			return response{}, -1, nil
		}
		parent := filepath.Dir(source)
		if parent == source || !within(parent, sourceRoot) {
			return response{}, -1, nil
		}
		source = parent
	}
	if within(m.directory, source) {
		return response{}, -1, errors.New("runtime state must be outside the source repository")
	}
	rel, _ := filepath.Rel(sourceRoot, source)
	target := filepath.Join(guestRoot, rel)
	repo := m.repositories[source]
	if repo == nil {
		privateRoot, err := os.MkdirTemp(m.directory, "repo-")
		if err != nil {
			return response{}, -1, err
		}
		if err := os.Chown(privateRoot, int(req.UID), int(req.GID)); err != nil {
			return response{}, -1, err
		}
		destination := filepath.Join(privateRoot, "worktree")
		var metadata unix.Stat_t
		if err := unix.Lstat(filepath.Join(source, ".git"), &metadata); err != nil {
			return response{}, -1, err
		}
		exe, err := os.Executable()
		if err != nil {
			return response{}, -1, err
		}
		cmd := exec.Command(exe, "prepare", source, destination)
		cmd.Env = gitEnvironment()
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: req.UID, Gid: req.GID, Groups: req.Groups}}
		if output, err := cmd.CombinedOutput(); err != nil {
			return response{}, -1, fmt.Errorf("prepare %s: %w: %s", source, err, output)
		}
		repo = &repository{Source: source, Directory: destination, UID: req.UID, GID: req.GID, Groups: req.Groups, GitDevice: uint64(metadata.Dev), GitInode: metadata.Ino}
		m.repositories[source] = repo
		data, err := json.MarshalIndent(m.repositories, "", "  ")
		if err != nil {
			return response{}, -1, err
		}
		if err := os.WriteFile(filepath.Join(m.directory, "repositories.json"), append(data, '\n'), 0600); err != nil {
			return response{}, -1, err
		}
		log.Printf("private repository: %s -> %s", source, destination)
	}
	pair, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return response{}, -1, err
	}
	socket, err := unet.NewSocket(pair[1])
	if err != nil {
		unix.Close(pair[0])
		unix.Close(pair[1])
		return response{}, -1, err
	}
	server := lisafs.NewServer()
	conn, err := server.CreateConnection(socket, repo.Directory, fsgofer.ConnectionOpts(false), fsgofer.NewConnectionImpl(&fsgofer.Config{DonateMountPointFD: true}))
	if err != nil {
		socket.Close()
		unix.Close(pair[0])
		server.Destroy()
		return response{}, -1, err
	}
	server.StartConnection(conn)
	m.stops = append(m.stops, func() { socket.Close(); server.Wait(); server.Destroy() })
	return response{Path: target}, pair[0], nil
}
