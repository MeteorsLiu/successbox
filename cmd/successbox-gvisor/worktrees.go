package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// The host root is the sandbox's lower layer. Hide runtime sockets, manifests
// and other sandboxes' private repositories before starting any guest code.
func hideWorktreeState(spec *specs.Spec, state string) error {
	state, err := filepath.EvalSymlinks(state)
	if err != nil {
		return err
	}
	sources := []specs.Mount{{Source: spec.Root.Path, Destination: "/", Type: "bind"}}
	sources = append(sources, spec.Mounts...)
	for _, mount := range sources {
		if mount.Type != "bind" {
			continue
		}
		source, err := filepath.EvalSymlinks(mount.Source)
		if err != nil {
			return err
		}
		inside, err := filepath.Rel(state, source)
		if err != nil {
			return err
		}
		if inside != ".." && !strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			spec.Mounts = append(spec.Mounts, specs.Mount{Destination: mount.Destination, Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "noexec", "mode=000"}})
			continue
		}
		relative, err := filepath.Rel(source, state)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			continue
		}
		spec.Mounts = append(spec.Mounts, specs.Mount{Destination: filepath.Join(mount.Destination, relative), Type: "tmpfs", Source: "tmpfs", Options: []string{"nosuid", "nodev", "noexec", "mode=000"}})
	}
	return nil
}

func (s *server) startWorktrees(ctx context.Context, id, dir string, log *os.File) (*exec.Cmd, error) {
	results := filepath.Join(s.root, "results", id)
	if err := os.MkdirAll(results, 0711); err != nil {
		return nil, err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer read.Close()
	defer write.Close()
	cmd := exec.Command(s.worktreeHelper, "--socket", filepath.Join(dir, "worktrees.sock"), "--bundle", filepath.Join(dir, "worktrees"), "--state", results, "--ready-fd", "3")
	cmd.Stdout, cmd.Stderr = log, log
	cmd.ExtraFiles = []*os.File{write}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	write.Close()
	stop := context.AfterFunc(ctx, func() { cmd.Process.Signal(syscall.SIGTERM) })
	defer stop()
	var ready [1]byte
	if _, err := io.ReadFull(read, ready[:]); err != nil || ready[0] != 1 {
		cmd.Process.Signal(syscall.SIGTERM)
		waitErr := cmd.Wait()
		diagnostic, _ := os.ReadFile(log.Name())
		if len(diagnostic) > 4096 {
			diagnostic = diagnostic[len(diagnostic)-4096:]
		}
		return nil, fmt.Errorf("helper readiness: %v; exit: %v: %s", err, waitErr, diagnostic)
	}
	return cmd, nil
}

// The Linux implementation owns host mount setup. The command remains buildable
// on macOS, where main directs callers to the Apple backend.
var worktreeNamespace = func() error { return errors.New("worktrees require Linux") }
var worktreeMounts = func(*specs.Spec, string) (func() error, error) { return nil, errors.New("worktrees require Linux") }
