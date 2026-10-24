//go:build linux

package main

import (
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	source := filepath.Join(base, "source")
	private := filepath.Join(base, "private")
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
	source, private := fixture(t)
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
	if err := prepare(source, private); err != nil {
		t.Fatal(err)
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
	if err := os.WriteFile(filepath.Join(private, ".git", "config"), []byte("[include]\npath = /definitely/not/available\n[core]\nworktree = /\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := publish(&repository{Source: source, Directory: private}); err != nil {
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
}

func TestPublishRejectsReplacedMetadata(t *testing.T) {
	source, private := fixture(t)
	if err := prepare(source, private); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(private, ".git"), filepath.Join(private, "saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(source, ".git"), filepath.Join(private, ".git")); err != nil {
		t.Fatal(err)
	}
	if err := publish(&repository{Source: source, Directory: private}); err == nil {
		t.Fatal("accepted symlink .git")
	}
}

func TestPrepareLinkedWorktree(t *testing.T) {
	source, private := fixture(t)
	linked := filepath.Join(filepath.Dir(source), "linked")
	if _, err := git(source, "worktree", "add", "--detach", linked); err != nil {
		t.Fatal(err)
	}
	if err := prepare(linked, private); err != nil {
		t.Fatal(err)
	}
	if err := publish(&repository{Source: linked, Directory: private}); err != nil {
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
			response, fd, err := m.prepare(request{Path: "/project/file"})
			if err != nil || fd != -1 || response.Path != "" || len(m.repositories) != 0 {
				t.Fatalf("response=%+v fd=%d err=%v", response, fd, err)
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
	if err := prepare(source, private); err != nil {
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
