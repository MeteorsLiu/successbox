//go:build linux

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() {
	if len(os.Args) == 7 && (os.Args[1] == "prepare" || os.Args[1] == "publish") {
		main()
		os.Exit(0)
	}
}

func fixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	source := filepath.Join(base, "source")
	private := filepath.Join(base, "repo-1", "worktree")
	for _, dir := range []string{filepath.Dir(private), filepath.Join(base, "common")} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := git(source, "init"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("initial\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := git(source, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := git(source, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial"); err != nil {
		t.Fatal(err)
	}
	return source, private
}

func TestPreparePublish(t *testing.T) {
	for _, kind := range []string{"ordinary", "split-index", "explicit-worktree", "alternates", "partial-sparse-shallow"} {
		t.Run(kind, func(t *testing.T) {
			source, private := fixture(t)
			if kind == "partial-sparse-shallow" {
				if err := os.Mkdir(filepath.Join(source, "unused"), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(source, "unused", "blob"), []byte("not downloaded\n"), 0644); err != nil {
					t.Fatal(err)
				}
				for _, args := range [][]string{
					{"add", "."},
					{"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "unused blob"},
					{"config", "uploadpack.allowFilter", "true"},
				} {
					if _, err := git(source, args...); err != nil {
						t.Fatal(err)
					}
				}
				partial := filepath.Join(filepath.Dir(source), "partial")
				if _, err := git("/", "clone", "--filter=blob:none", "--depth=1", "--no-checkout", "file://"+source, partial); err != nil {
					t.Fatal(err)
				}
				for _, args := range [][]string{{"sparse-checkout", "set", "--no-cone", "/file"}, {"checkout"}} {
					if _, err := git(partial, args...); err != nil {
						t.Fatal(err)
					}
				}
				// Preparation must succeed without obtaining missing objects from upstream.
				if err := os.Rename(source, source+"-offline"); err != nil {
					t.Fatal(err)
				}
				source = partial
			}
			if kind == "alternates" {
				for _, name := range []string{"shared1", "shared2"} {
					shared := filepath.Join(filepath.Dir(source), name)
					if _, err := git("/", "clone", "--shared", source, shared); err != nil {
						t.Fatal(err)
					}
					source = shared
				}
			}
			if kind == "explicit-worktree" {
				if _, err := git(source, "config", "core.worktree", source); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "split-index" {
				if _, err := git(source, "update-index", "--split-index"); err != nil {
					t.Fatal(err)
				}
			}
			originalHead, err := git(source, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "file"), []byte("staged\n"), 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := git(source, "add", "file"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "file"), []byte("dirty\n"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source, "untracked"), []byte("untracked\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("file", filepath.Join(source, "link")); err != nil {
				t.Fatal(err)
			}
			before, err := git(source, "status", "--porcelain")
			if err != nil {
				t.Fatal(err)
			}
			registrations, err := git(source, "worktree", "list", "--porcelain")
			if err != nil {
				t.Fatal(err)
			}
			if err := prepare(source, private, filepath.Join(filepath.Dir(source), "common"), filepath.Dir(source), true); err != nil {
				t.Fatal(err)
			}
			if got, err := git(source, "worktree", "list", "--porcelain"); err != nil || got != registrations {
				t.Fatalf("preparation changed host worktree registrations: %q: %v", got, err)
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(source), "common", "objects", "info", "alternates")); !os.IsNotExist(err) {
				t.Fatalf("private object store still uses alternates: %v", err)
			}
			if kind == "partial-sparse-shallow" {
				for _, args := range [][]string{{"rev-parse", "--is-shallow-repository"}, {"sparse-checkout", "list"}, {"rev-list", "--objects", "--missing=print", "HEAD"}} {
					want, err := git(source, args...)
					if err != nil {
						t.Fatal(err)
					}
					got, err := git(private, args...)
					if err != nil || got != want {
						t.Fatalf("%v: private=%q source=%q: %v", args, got, want, err)
					}
					if args[0] == "rev-list" && !strings.Contains(got, "\n?") {
						t.Fatal("fixture must retain a missing blob")
					}
				}
			}
			after, err := git(private, "status", "--porcelain")
			if err != nil || after != before {
				t.Fatalf("private status %q, want %q: %v", after, before, err)
			}
			if err := os.WriteFile(filepath.Join(private, "file"), []byte("guest\n"), 0755); err != nil {
				t.Fatal(err)
			}
			if _, err := git(private, "add", "file"); err != nil {
				t.Fatal(err)
			}
			if _, err := git(private, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "guest"); err != nil {
				t.Fatal(err)
			}
			guestHead, err := git(private, "rev-parse", "HEAD")
			if err != nil {
				t.Fatal(err)
			}
			// This config must never be evaluated by host-side publication commands.
			if err := os.WriteFile(filepath.Join(filepath.Dir(source), "common", "config"), []byte("[include]\npath = /definitely/not/available\n[core]\nworktree = /\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := publish(&repository{Source: source, Directory: private, CommonDirectory: filepath.Join(filepath.Dir(source), "common"), GuestRoot: filepath.Dir(source)}); err != nil {
				t.Fatal(err)
			}
			got, err := git(source, "worktree", "list", "--porcelain")
			if err != nil || !strings.Contains(got, "worktree "+private+"\nHEAD "+guestHead) {
				t.Fatalf("worktree list: %q: %v", got, err)
			}
			got, err = git(source, "rev-parse", "HEAD")
			if err != nil || got != originalHead {
				t.Fatalf("source HEAD changed: %q: %v", got, err)
			}
			got, err = git(source, "status", "--porcelain")
			if err != nil || got != before {
				t.Fatalf("source status changed: %q, want %q: %v", got, before, err)
			}
			data, err := os.ReadFile(filepath.Join(source, "file"))
			if err != nil || string(data) != "dirty\n" {
				t.Fatalf("source contents: %q: %v", data, err)
			}
			got, err = git(private, "show", ":file")
			if err != nil || got != "guest" {
				t.Fatalf("published index: %q: %v", got, err)
			}
			if link, err := os.Readlink(filepath.Join(private, "link")); err != nil || link != "file" {
				t.Fatalf("symlink: %q: %v", link, err)
			}
		})
	}
}

func TestPublishRejectsReplacedMetadata(t *testing.T) {
	source, private := fixture(t)
	if err := prepare(source, private, filepath.Join(filepath.Dir(source), "common"), filepath.Dir(source), true); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(private, ".git"), filepath.Join(private, "saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, ".git"), filepath.Join(private, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := publish(&repository{Source: source, Directory: private, CommonDirectory: filepath.Join(filepath.Dir(source), "common"), GuestRoot: filepath.Dir(source)}); err == nil {
		t.Fatal("accepted symlink .git")
	}
}

func TestPrepareLinkedWorktree(t *testing.T) {
	source, private := fixture(t)
	linked := filepath.Join(filepath.Dir(source), "linked")
	if _, err := git(source, "worktree", "add", "--detach", linked); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "file"), []byte("linked commit\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "file"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "linked"}, {"update-index", "--split-index"}} {
		if _, err := git(linked, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(linked, "file"), []byte("linked dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "extensions.worktreeConfig", "true"}, {"config", "--worktree", "test.mainOnly", "true"}} {
		if _, err := git(source, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := prepare(linked, private, filepath.Join(filepath.Dir(source), "common"), filepath.Dir(source), true); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"rev-parse", "HEAD"}, {"show", ":file"}, {"status", "--porcelain"}} {
		want, err := git(linked, args...)
		if err != nil {
			t.Fatal(err)
		}
		got, err := git(private, args...)
		if err != nil || got != want {
			t.Fatalf("%v: private=%q source=%q: %v", args, got, want, err)
		}
	}
	if dirs, err := os.ReadDir(filepath.Join(filepath.Dir(source), "common", "worktrees")); err != nil || len(dirs) != 1 {
		t.Fatalf("private registrations: %v: %v", dirs, err)
	}
	if value, err := git(private, "config", "--get", "test.mainOnly"); err == nil || value != "" {
		t.Fatalf("inherited another worktree's local config: %q: %v", value, err)
	}
	if err := publish(&repository{Source: linked, Directory: private, CommonDirectory: filepath.Join(filepath.Dir(source), "common"), GuestRoot: filepath.Dir(source)}); err != nil {
		t.Fatal(err)
	}
	got, err := git(source, "worktree", "list", "--porcelain")
	if err != nil || !strings.Contains(got, "worktree "+private+"\n") {
		t.Fatalf("worktree list: %q: %v", got, err)
	}
}

func TestDiscoveryMountBoundaries(t *testing.T) {
	source, _ := fixture(t)
	for _, tc := range []struct {
		name  string
		mount specs.Mount
	}{
		{"read-only", specs.Mount{Source: source, Destination: "/project", Type: "bind", Options: []string{"ro"}}},
		{"tmpfs", specs.Mount{Destination: "/project", Type: "tmpfs"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &manager{root: source, directory: t.TempDir(), mounts: []specs.Mount{tc.mount}, repositories: make(map[string]*repository)}
			path, err := m.prepare(request{Path: "/project/file"})
			if err != nil || path != "" || len(m.repositories) != 0 {
				t.Fatalf("path=%q err=%v", path, err)
			}
		})
	}
}

func TestPrepareReadOnlyDirectory(t *testing.T) {
	source, private := fixture(t)
	locked := filepath.Join(source, "locked")
	if err := os.Mkdir(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "file"), []byte("readable"), 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700); _ = os.Chmod(filepath.Join(private, "locked"), 0700) })
	if err := prepare(source, private, filepath.Join(filepath.Dir(source), "common"), filepath.Dir(source), true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(private, "locked"))
	if err != nil || info.Mode().Perm() != 0500 {
		t.Fatalf("directory mode: %v %v", info, err)
	}
	if data, err := os.ReadFile(filepath.Join(private, "locked", "file")); err != nil || string(data) != "readable" {
		t.Fatalf("contents: %q %v", data, err)
	}
}

func TestSharedPrivateWorktrees(t *testing.T) {
	source, first := fixture(t)
	base := filepath.Dir(source)
	linked := filepath.Join(base, "linked")
	if _, err := git(source, "worktree", "add", "--detach", linked); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(linked, "file"), []byte("linked staged\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := git(linked, "add", "file"); err != nil {
		t.Fatal(err)
	}
	common := filepath.Join(base, "common")
	second := filepath.Join(base, "repo-2", "worktree")
	if err := prepare(source, first, common, base, true); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(second), 0755); err != nil {
		t.Fatal(err)
	}
	if err := prepare(linked, second, common, base, false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{first, second} {
		got, err := git(p, "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil || filepath.Clean(got) != common {
			t.Fatalf("common = %q: %v", got, err)
		}
	}
	if got, err := git(first, "show", ":file"); err != nil || got != "initial" {
		t.Fatalf("first index: %q: %v", got, err)
	}
	if got, err := git(second, "show", ":file"); err != nil || got != "linked staged" {
		t.Fatalf("second index: %q: %v", got, err)
	}
	if _, err := git(second, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "private change"); err != nil {
		t.Fatal(err)
	}
	commit, err := git(second, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := git(source, "cat-file", "-e", commit); err == nil {
		t.Fatal("private commit reached host before publication")
	}
	if _, err := git(first, "cat-file", "-e", commit); err != nil {
		t.Fatal("private worktrees do not share object data:", err)
	}
	// The explicit guest-root prefix is tested by real interceptor integration;
	// these direct Git calls use the host prefix as their guest view.
	// Git maintenance can leave this lock after a guest commit. It is not an
	// object and must neither prevent publication nor be copied to the host.
	if err := os.WriteFile(filepath.Join(common, "objects", "maintenance.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := publish(&repository{Source: source, Directory: first, CommonDirectory: common, GuestRoot: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(source, ".git", "objects", "maintenance.lock")); !os.IsNotExist(err) {
		t.Fatalf("guest maintenance lock reached the source: %v", err)
	}
	if err := publish(&repository{Source: linked, Directory: second, CommonDirectory: common, GuestRoot: base, skipObjects: true}); err != nil {
		t.Fatal(err)
	}
	if got, err := git(second, "rev-parse", "HEAD"); err != nil || got != commit {
		t.Fatalf("published commit: %q: %v", got, err)
	}
	if got, err := git(source, "show", ":file"); err != nil || got != "initial" {
		t.Fatalf("host index: %q: %v", got, err)
	}
}

func TestConcurrentRepositoryPreparation(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("manager preparation uses rootful credential workers")
	}
	source, _ := fixture(t)
	base := filepath.Dir(source)
	linked := filepath.Join(base, "linked")
	if _, err := git(source, "worktree", "add", "--detach", linked); err != nil {
		t.Fatal(err)
	}
	m := &manager{root: base, directory: t.TempDir()}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := "source"
			if i%2 == 1 {
				name = "linked"
			}
			path, err := m.prepare(request{Path: "/" + name + "/file"})
			if err != nil {
				t.Error(err)
				return
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != "initial\n" {
				t.Errorf("prepared data: %q: %v", data, err)
			}
		}(i)
	}
	wg.Wait()
	if len(m.repositories) != 2 || len(m.common) != 1 {
		t.Fatalf("repositories=%d common=%d", len(m.repositories), len(m.common))
	}
	manifest, err := os.ReadFile(filepath.Join(m.directory, "repositories.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]repository
	if err := json.Unmarshal(manifest, &saved); err != nil || len(saved) != 2 {
		t.Fatalf("manifest: %s: %v", manifest, err)
	}
}

func TestPreparationDoesNotBlockOtherPaths(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("manager preparation uses rootful credential workers")
	}
	source, _ := fixture(t)
	common := &commonRepository{ready: make(chan struct{})}
	// Hold this repository's common-data copy in progress.
	key := fmt.Sprintf("%s\x00%d:%d:%v", filepath.Join(source, ".git"), 0, 0, []uint32(nil))
	m := &manager{root: filepath.Dir(source), directory: t.TempDir(), common: map[string]*commonRepository{key: common}}
	done := make(chan error, 1)
	go func() { _, err := m.prepare(request{Path: "/source/file"}); done <- err }()
	defer func() { common.err = errors.New("copy stopped by test"); close(common.ready); <-done }()
	deadline := time.After(5 * time.Second)
	for {
		m.mu.Lock()
		pending := len(m.repositories) == 1
		m.mu.Unlock()
		if pending {
			break
		}
		select {
		case <-deadline:
			t.Fatal("preparation did not start")
		case <-time.After(time.Millisecond):
		}
	}
	other := make(chan error, 1)
	go func() { _, err := m.prepare(request{Path: "/ordinary.txt"}); other <- err }()
	select {
	case err := <-other:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("unrelated path blocked behind repository preparation")
	}
}
