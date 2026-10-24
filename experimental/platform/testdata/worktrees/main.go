//go:build linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"gvisor.dev/gvisor/sandboxexec/sandbox"
)

func main() {
	if len(os.Args) == 4 && os.Args[1] == "guest" {
		source := os.Args[2]
		parent, err := unix.Open(filepath.Dir(source), unix.O_DIRECTORY|unix.O_RDONLY, 0)
		if err != nil {
			panic(err)
		}
		defer unix.Close(parent)
		number, err := unix.Openat(parent, filepath.Base(source)+"/dirfd-"+os.Args[3], unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC, 0644)
		if err != nil {
			panic(err)
		}
		file := os.NewFile(uintptr(number), "dirfd")
		if _, err := file.WriteString("guest"); err != nil {
			panic(err)
		}
		if err := file.Close(); err != nil {
			panic(err)
		}
		data, err := os.ReadFile(source + "/link/../value")
		if err != nil || string(data) != "resolved" {
			panic(fmt.Sprintf("symlink parent: %q %v", data, err))
		}
		return
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() (err error) {
	if len(os.Args) != 4 {
		return fmt.Errorf("usage: worktree-probe DIRECTORY HELPER RUNSC")
	}
	dir, helper, runtime := os.Args[1], os.Args[2], os.Args[3]
	source := filepath.Join(dir, "source")
	state := filepath.Join(dir, "results")
	for _, p := range []string{source, state, filepath.Join(dir, "overlays"), filepath.Join(dir, "debug")} {
		if err := os.MkdirAll(p, 0755); err != nil {
			return err
		}
	}
	git := func(args ...string) error {
		cmd := exec.Command("git", append([]string{"-C", source}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git: %w: %s", err, out)
		}
		return nil
	}
	if err := git("init"); err != nil {
		return err
	}
	for _, name := range []string{"tracked", "delete", "rename"} {
		if err := os.WriteFile(filepath.Join(source, name), []byte("original\n"), 0644); err != nil {
			return err
		}
	}
	if err := git("add", "."); err != nil {
		return err
	}
	if err := git("-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "fixture"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(source, "tracked"), []byte("host staged\n"), 0644); err != nil {
		return err
	}
	if err := git("add", "tracked"); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(source, "tracked"), []byte("host dirty\n"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(source, "untracked"), []byte("host untracked\n"), 0644); err != nil {
		return err
	}
	if err := os.Symlink(source, filepath.Join(dir, "alias")); err != nil {
		return err
	}
	other := filepath.Join(dir, "other")
	clone := exec.Command("git", "clone", "--no-hardlinks", source, other)
	if out, err := clone.CombinedOutput(); err != nil {
		return fmt.Errorf("second fixture: %w %s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(other, "real", "sub"), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(other, "real", "value"), []byte("resolved"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(other, "value"), []byte("lexical parent is wrong"), 0644); err != nil {
		return err
	}
	if err := os.Symlink("real/sub", filepath.Join(other, "link")); err != nil {
		return err
	}
	config, _ := json.Marshal(map[string]any{"root": map[string]string{"path": "/"}})
	if err := os.WriteFile(filepath.Join(dir, "config.json"), config, 0600); err != nil {
		return err
	}
	read, write, err := os.Pipe()
	if err != nil {
		return err
	}
	defer read.Close()
	manager := exec.Command(helper, "--socket", filepath.Join(dir, "worktrees.sock"), "--bundle", dir, "--state", state, "--ready-fd", "3")
	manager.Stdout, manager.Stderr = os.Stdout, os.Stderr
	manager.ExtraFiles = []*os.File{write}
	if err := manager.Start(); err != nil {
		return err
	}
	write.Close()
	defer func() {
		manager.Process.Signal(syscall.SIGTERM)
		if e := manager.Wait(); err == nil {
			err = e
		}
	}()
	ready := make([]byte, 1)
	if _, err := read.Read(ready); err != nil {
		return err
	}
	wrapper := filepath.Join(dir, "runsc")
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	contents := fmt.Sprintf("#!/bin/sh\nexec %s --ignore-cgroups --platform=successbox --platform_device_path=%s --overlay2=all:dir=%s \"$@\"\n", quote(runtime), quote(filepath.Join(dir, "worktrees.sock")), quote(filepath.Join(dir, "overlays")))
	if err := os.WriteFile(wrapper, []byte(contents), 0700); err != nil {
		return err
	}
	if err := os.Setenv("RUNSC_PATH", wrapper); err != nil {
		return err
	}
	ctx := context.Background()
	box, err := sandbox.New(ctx, sandbox.WithRootfs("/", false), sandbox.WithRuntimeDir(filepath.Join(dir, "runtime")), sandbox.WithStateDir(filepath.Join(dir, "sandbox-state")), sandbox.WithDebug(filepath.Join(dir, "debug")+"/"), sandbox.WithoutHostBinaryMounts())
	if err != nil {
		return err
	}
	defer func() {
		if e := box.Close(ctx); err == nil {
			err = e
		}
	}()
	if _, err := os.Stat(filepath.Join(state, "repositories.json")); !os.IsNotExist(err) {
		return fmt.Errorf("repository prepared before first access: %v", err)
	}
	script := `set -eu
cd "$1"
test "$(cat tracked)" = 'host dirty'
test "$(git show :tracked)" = 'host staged'
test "$(cat untracked)" = 'host untracked'
printf 'guest\n' > tracked
mv rename renamed
rm delete
printf 'new\n' > created
git status --porcelain
`
	result, err := box.Exec(ctx, []string{"/bin/sh", "-c", script, "probe", filepath.Join(dir, "alias")})
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("guest edits: %+v: %v", result, err)
	}
	fmt.Print(result.Stdout)
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	var calls sync.WaitGroup
	failures := make(chan error, 8)
	for i := range 8 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			result, err := box.Exec(ctx, []string{executable, "guest", other, fmt.Sprint(i)})
			if err != nil || result.ExitCode != 0 {
				failures <- fmt.Errorf("concurrent call %d: %+v: %v", i, result, err)
			}
		}()
	}
	calls.Wait()
	close(failures)
	for err := range failures {
		return err
	}
	data, err := os.ReadFile(filepath.Join(source, "tracked"))
	if err != nil || string(data) != "host dirty\n" {
		return fmt.Errorf("source changed: %q: %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(source, "delete")); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(source, "created")); !os.IsNotExist(err) {
		return fmt.Errorf("guest file leaked: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(state, "repositories.json"))
	if err != nil {
		return err
	}
	var repos map[string]struct{ Source, Directory string }
	if err := json.Unmarshal(data, &repos); err != nil {
		return err
	}
	if len(repos) != 2 {
		return fmt.Errorf("prepared %d repositories", len(repos))
	}
	for _, repo := range repos {
		if repo.Source != source {
			continue
		}
		data, err := os.ReadFile(filepath.Join(repo.Directory, "tracked"))
		if err != nil || strings.TrimSpace(string(data)) != "guest" {
			return fmt.Errorf("private edit missing: %q: %v", data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(other, "dirfd-0")); !os.IsNotExist(err) {
		return fmt.Errorf("dirfd write reached source: %v", err)
	}
	result, err = box.Exec(ctx, []string{"/bin/sh", "-c", "printf discarded > /etc/successbox-worktree-test"})
	if err != nil || result.ExitCode != 0 {
		return fmt.Errorf("system overlay: %+v %v", result, err)
	}
	if _, err := os.Stat("/etc/successbox-worktree-test"); !os.IsNotExist(err) {
		return fmt.Errorf("system write reached host: %v", err)
	}
	fmt.Println("worktrees: dirty/staged/untracked files, guest Git, rename/delete, symlink/.., dirfd access, 8 simultaneous first accesses and system-write isolation passed")
	return nil
}
