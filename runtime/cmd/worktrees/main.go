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
	CommonDirectory     string
	GuestRoot           string
	ready               chan struct{}
	err                 error
	skipObjects         bool
}
type commonRepository struct {
	directory string
	ready     chan struct{}
	err       error
}
type manager struct {
	root, directory string
	guestDirectory  string
	mounts          []specs.Mount
	mu              sync.Mutex
	repositories    map[string]*repository
	mappings        map[string]string
	common          map[string]*commonRepository
}

func main() {
	if len(os.Args) == 7 && os.Args[1] == "publish" {
		if err := publish(&repository{Source: os.Args[2], Directory: os.Args[3], CommonDirectory: os.Args[4], skipObjects: os.Args[5] == "skip", GuestRoot: os.Args[6]}); err != nil {
			log.Fatal(err)
		}
		return
	}
	if len(os.Args) == 7 && os.Args[1] == "prepare" {
		if err := prepare(os.Args[2], os.Args[3], os.Args[4], os.Args[6], os.Args[5] == "new"); err != nil {
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
	imported := make(map[string]bool)
	for _, repo := range m.repositories {
		if repo.err != nil {
			log.Printf("repository preparation failed: %s: %v", repo.Source, repo.err)
			failed = true
			continue
		}
		var metadata unix.Stat_t
		err := unix.Lstat(filepath.Join(repo.Source, ".git"), &metadata)
		if err == nil && (uint64(metadata.Dev) != repo.GitDevice || metadata.Ino != repo.GitInode) {
			err = errors.New("original repository identity changed")
		}
		exe, exeErr := os.Executable()
		err = errors.Join(err, exeErr)
		if err == nil {
			objects := "import"
			if imported[repo.CommonDirectory] {
				objects = "skip"
			}
			cmd := exec.Command(exe, "publish", repo.Source, repo.Directory, repo.CommonDirectory, objects, repo.GuestRoot)
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
		} else {
			imported[repo.CommonDirectory] = true
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
	m.mu.Lock()
	if m.repositories == nil {
		m.repositories = make(map[string]*repository)
	}
	repo := m.repositories[source]
	first := repo == nil
	if first {
		repo = &repository{Source: source, UID: req.UID, GID: req.GID, Groups: req.Groups, ready: make(chan struct{})}
		m.repositories[source] = repo
	}
	m.mu.Unlock()
	if first {
		err := m.prepareRepository(repo)
		m.mu.Lock()
		repo.err = err
		if err == nil {
			completed := make(map[string]*repository)
			for path, entry := range m.repositories {
				if entry == repo {
					completed[path] = entry
					continue
				}
				select {
				case <-entry.ready:
					if entry.err == nil {
						completed[path] = entry
					}
				default:
				}
			}
			data, err := json.MarshalIndent(completed, "", "  ")
			if err == nil {
				err = os.WriteFile(filepath.Join(m.directory, "repositories.json"), append(data, '\n'), 0600)
			}
			repo.err = err
		}
		close(repo.ready)
		m.mu.Unlock()
	} else {
		<-repo.ready
	}
	if repo.err != nil {
		return "", repo.err
	}
	private := repo.Directory
	if m.guestDirectory != "" {
		private = m.guestDirectory + strings.TrimPrefix(repo.Directory, m.directory)
	}
	m.mu.Lock()
	if m.mappings == nil {
		m.mappings = make(map[string]string)
	}
	m.mappings[target] = private
	m.mu.Unlock()
	return private + strings.TrimPrefix(guest, target), nil
}

// Filesystem copying and subprocess waits never hold the path-mapping lock.
// Only requests for the same repository or common Git store wait for each other.
func (m *manager) prepareRepository(repo *repository) (err error) {
	privateRoot, err := os.MkdirTemp(m.directory, "repo-")
	if err != nil {
		return err
	}
	if err := os.Chown(privateRoot, int(repo.UID), int(repo.GID)); err != nil {
		return err
	}
	destination := filepath.Join(privateRoot, "worktree")
	var metadata unix.Stat_t
	if err := unix.Lstat(filepath.Join(repo.Source, ".git"), &metadata); err != nil {
		return err
	}
	credential := &syscall.Credential{Uid: repo.UID, Gid: repo.GID, Groups: repo.Groups}
	cmd := exec.Command("git", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", repo.Source, "rev-parse", "--path-format=absolute", "--git-common-dir")
	cmd.Env = gitEnvironment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("resolve common Git directory: %w: %s", err, output)
	}
	commonPath, err := filepath.EvalSymlinks(strings.TrimSpace(string(output)))
	if err != nil {
		return err
	}
	// Separate credentials must not acquire data through another user's copy.
	key := fmt.Sprintf("%s\x00%d:%d:%v", commonPath, repo.UID, repo.GID, repo.Groups)
	m.mu.Lock()
	if m.common == nil {
		m.common = make(map[string]*commonRepository)
	}
	common := m.common[key]
	initialize := common == nil
	if initialize {
		common = &commonRepository{ready: make(chan struct{})}
		m.common[key] = common
	}
	m.mu.Unlock()
	if initialize {
		defer func() { common.err = err; close(common.ready) }()
		common.directory, err = os.MkdirTemp(m.directory, "common-")
		if err != nil {
			return err
		}
		if err := os.Chown(common.directory, int(repo.UID), int(repo.GID)); err != nil {
			return err
		}
	} else {
		<-common.ready
		if common.err != nil {
			return common.err
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	mode := "reuse"
	if initialize {
		mode = "new"
	}
	guestRoot := m.guestDirectory
	if guestRoot == "" {
		guestRoot = m.directory
	}
	cmd = exec.Command(exe, "prepare", repo.Source, destination, common.directory, mode, guestRoot)
	cmd.Env = gitEnvironment()
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("prepare %s: %w: %s", repo.Source, err, output)
	}
	repo.Directory = destination
	repo.CommonDirectory = common.directory
	repo.GuestRoot = guestRoot
	repo.GitDevice, repo.GitInode = uint64(metadata.Dev), metadata.Ino
	log.Printf("private repository: %s -> %s (common %s)", repo.Source, destination, common.directory)
	return nil
}
