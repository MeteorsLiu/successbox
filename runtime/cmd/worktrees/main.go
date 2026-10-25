//go:build linux

// worktrees prepares private repositories for the successbox platform. Its
// socket is a runtime device, never a guest FD or a guest-visible service.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

type request struct {
	Path     string
	UID, GID uint32
	Groups   []uint32
}
type repository struct {
	Source, Directory   string
	UID, GID            uint32
	Groups              []uint32
	GitDevice, GitInode uint64
}
type manager struct {
	root, directory string
	guestDirectory  string
	mounts          []specs.Mount
	mu              sync.Mutex
	repositories    map[string]*repository
	mappings        map[string]string
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
	// Preparation and publication use host IDs. Rootless user-namespace ID
	// translation is not part of this command.
	if os.Geteuid() != 0 {
		log.Fatal("worktrees require a rootful backend")
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
	listener, err := net.ListenUnix("unixpacket", &net.UnixAddr{Name: *socket, Net: "unixpacket"})
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
	m := &manager{root: root, directory: *directory, mounts: spec.Mounts, repositories: make(map[string]*repository), mappings: make(map[string]string)}
	m.guestDirectory = m.directory
	for _, mount := range spec.Mounts {
		if mount.Type == "bind" && mount.Source == m.directory {
			m.guestDirectory = mount.Destination
			break
		}
	}
	if *readyFD >= 0 {
		f := os.NewFile(uintptr(*readyFD), "ready")
		if _, err := f.Write([]byte{1}); err != nil {
			log.Fatal(err)
		}
		f.Close()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer cancel()
	if err := serve(ctx, listener, m.inspect); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal(err)
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

func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, strings.TrimSuffix(root, "/")+"/")
}

func (m *manager) prepare(req request) (string, error) {
	if !filepath.IsAbs(req.Path) || strings.ContainsRune(req.Path, 0) {
		return "", errors.New("invalid guest path")
	}
	guest := filepath.Clean(req.Path)
	if m.guestDirectory != "" && within(guest, m.guestDirectory) {
		return guest, nil
	}
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
		return "", nil
	}
	if mapped := m.mapped(guest); mapped != "" {
		return mapped, nil
	}
	relative, _ := filepath.Rel(guestRoot, guest)
	source := filepath.Join(sourceRoot, relative)
	if within(source, m.directory) {
		return "", nil
	}
	// Paths come from Sentry's permission-checked, symlink-resolved VFS lookup.
	// Never search above the root of the host mount that supplied that path.
	for {
		fi, err := os.Lstat(filepath.Join(source, ".git"))
		if err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			break
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, unix.ENOTDIR) {
			return "", err
		}
		if source == sourceRoot {
			return "", nil
		}
		parent := filepath.Dir(source)
		if parent == source || !within(parent, sourceRoot) {
			return "", nil
		}
		source = parent
	}
	if within(m.directory, source) {
		return "", errors.New("runtime state must be outside the source repository")
	}
	rel, _ := filepath.Rel(sourceRoot, source)
	target := filepath.Join(guestRoot, rel)
	repo := m.repositories[source]
	if repo == nil {
		privateRoot, err := os.MkdirTemp(m.directory, "repo-")
		if err != nil {
			return "", err
		}
		if err := os.Chown(privateRoot, int(req.UID), int(req.GID)); err != nil {
			return "", err
		}
		destination := filepath.Join(privateRoot, "worktree")
		var metadata unix.Stat_t
		if err := unix.Lstat(filepath.Join(source, ".git"), &metadata); err != nil {
			return "", err
		}
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		cmd := exec.Command(exe, "prepare", source, destination)
		cmd.Env = gitEnvironment()
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: req.UID, Gid: req.GID, Groups: req.Groups}}
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("prepare %s: %w: %s", source, err, output)
		}
		repo = &repository{Source: source, Directory: destination, UID: req.UID, GID: req.GID, Groups: req.Groups, GitDevice: uint64(metadata.Dev), GitInode: metadata.Ino}
		m.repositories[source] = repo
		data, err := json.MarshalIndent(m.repositories, "", "  ")
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(m.directory, "repositories.json"), append(data, '\n'), 0600); err != nil {
			return "", err
		}
		log.Printf("private repository: %s -> %s", source, destination)
	}
	if m.mappings == nil {
		m.mappings = make(map[string]string)
	}
	private := repo.Directory
	if m.guestDirectory != "" {
		private = m.guestDirectory + strings.TrimPrefix(repo.Directory, m.directory)
	}
	m.mappings[target] = private
	return private + strings.TrimPrefix(guest, target), nil
}
