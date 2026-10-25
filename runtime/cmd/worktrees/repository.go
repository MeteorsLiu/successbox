//go:build linux

package main

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func gitEnvironment() []string {
	return []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/nonexistent", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0"}
}

func git(directory string, args ...string) (string, error) {
	options := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "maintenance.auto=false", "-c", "gc.auto=0"}
	cmd := exec.Command("git", append(options, args...)...)
	cmd.Dir = directory
	cmd.Env = gitEnvironment()
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, output)
	}
	return strings.TrimSpace(string(output)), nil
}

func prepare(source, destination, commonDirectory, guestRoot string, initialize bool) error {
	common, err := git(source, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	gitdir, err := git(source, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	box, err := os.OpenRoot(filepath.Dir(commonDirectory))
	if err != nil {
		return err
	}
	defer box.Close()
	shared, err := box.OpenRoot(filepath.Base(commonDirectory))
	if err != nil {
		return err
	}
	defer shared.Close()
	if initialize {
		if err := copyGitMetadata(common, shared, true); err != nil {
			return err
		}
		if err := copyFile(filepath.Join(common, "HEAD"), "HEAD", shared); err != nil {
			return err
		}
		objects, err := git(source, "count-objects", "-v")
		if err != nil {
			return err
		}
		for _, line := range strings.Split(objects, "\n") {
			if !strings.HasPrefix(line, "alternate: ") {
				continue
			}
			path := strings.TrimPrefix(line, "alternate: ")
			if strings.HasPrefix(path, "\"") {
				path, err = strconv.Unquote(path)
				if err != nil {
					return err
				}
			}
			// Copy recursive alternate stores once; no host object path reaches guest.
			if err := filepath.WalkDir(path, func(file string, entry fs.DirEntry, err error) error {
				if err != nil {
					return err
				}
				rel, err := filepath.Rel(path, file)
				if err != nil {
					return err
				}
				if rel == "info/alternates" {
					return nil
				}
				target := filepath.Join("objects", rel)
				if entry.IsDir() {
					return shared.MkdirAll(target, 0755)
				}
				if !entry.Type().IsRegular() {
					return fmt.Errorf("non-regular Git object: %s", file)
				}
				if _, err := shared.Lstat(target); err == nil {
					return nil
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
				return copyFile(file, target, shared)
			}); err != nil {
				return err
			}
		}
	}
	name := filepath.Base(filepath.Dir(destination))
	adminPath := filepath.Join("worktrees", name)
	if err := shared.MkdirAll("worktrees", 0755); err != nil {
		return err
	}
	if err := shared.Mkdir(adminPath, 0755); err != nil {
		return err
	}
	admin, err := shared.OpenRoot(adminPath)
	if err != nil {
		return err
	}
	defer admin.Close()
	if err := copyGitMetadata(gitdir, admin, false); err != nil {
		return err
	}

	// Git config editing only sees a new host temporary file, never config that
	// an already-running guest could have changed in the shared repository.
	for _, scope := range []struct {
		root     *os.Root
		name     string
		source   string
		settings [][2]string
	}{
		{shared, "config", filepath.Join(common, "config"), [][2]string{{"core.bare", "true"}, {"extensions.worktreeConfig", "true"}}},
		{admin, "config.worktree", filepath.Join(gitdir, "config.worktree"), [][2]string{{"core.bare", "false"}}},
	} {
		if scope.root == shared && !initialize {
			continue
		}
		data, err := os.ReadFile(scope.source)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		config, err := os.CreateTemp("", "successbox-git-config-")
		if err != nil {
			return err
		}
		defer os.Remove(config.Name())
		_, writeErr := config.Write(data)
		if err := errors.Join(writeErr, config.Close()); err != nil {
			return err
		}
		// Remove a host worktree path even if the original checkout was relocated.
		if _, err := git("/", "config", "--file", config.Name(), "--replace-all", "core.worktree", "."); err != nil {
			return err
		}
		if _, err := git("/", "config", "--file", config.Name(), "--unset-all", "core.worktree"); err != nil {
			return err
		}
		for _, setting := range scope.settings {
			if _, err := git("/", "config", "--file", config.Name(), "--replace-all", setting[0], setting[1]); err != nil {
				return err
			}
		}
		if err := copyFile(config.Name(), scope.name, scope.root); err != nil {
			return err
		}
	}
	worktreePath, err := filepath.Rel(filepath.Dir(commonDirectory), destination)
	if err != nil {
		return err
	}
	if err := box.MkdirAll(worktreePath, 0755); err != nil {
		return err
	}
	worktree, err := box.OpenRoot(worktreePath)
	if err != nil {
		return err
	}
	defer worktree.Close()
	forward := filepath.Join(guestRoot, filepath.Base(commonDirectory), adminPath)
	backward := filepath.Join(guestRoot, worktreePath, ".git")
	// getcwd exposes the original logical path. Git resolves .git references
	// against that path, so relative references to the private directory fail.
	// Use the already-mounted guest results path in both directions instead.
	if err := admin.WriteFile("commondir", []byte("../..\n"), 0644); err != nil {
		return err
	}
	if err := admin.WriteFile("gitdir", []byte(backward+"\n"), 0644); err != nil {
		return err
	}
	if err := worktree.WriteFile(".git", []byte("gitdir: "+forward+"\n"), 0644); err != nil {
		return err
	}
	return copyTree(source, worktree, true)
}

// Copy common data once, and each worktree's HEAD/index/config separately.
// No Git command evaluates private metadata while another guest can edit it.
func copyGitMetadata(source string, destination *os.Root, shared bool) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if rel == "worktrees" || rel == "gitdir" || rel == "commondir" || rel == "objects/info/alternates" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// These directories contain both common and per-worktree records.
		if rel == "refs" || rel == "logs" || rel == "info" {
			return destination.MkdirAll(rel, 0755)
		}
		top := strings.SplitN(rel, "/", 2)[0]
		common := false
		switch top {
		case "objects", "refs", "logs", "info", "config", "packed-refs", "shallow", "hooks", "description", "rr-cache", "modules", "branches", "remotes":
			common = true
		}
		if rel == "logs/HEAD" || rel == "info/sparse-checkout" || within(rel, "refs/bisect") || within(rel, "refs/worktree") || within(rel, "refs/rewritten") {
			common = false
		}
		if common != shared {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return destination.MkdirAll(rel, 0755)
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("Git metadata contains unsupported file %s (%s)", path, entry.Type())
		}
		return copyFile(path, rel, destination)
	})
}

func copyFile(source, destination string, root *os.Root) error {
	input, err := os.OpenFile(source, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", source)
	}
	output, err := root.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|unix.O_NOFOLLOW, info.Mode().Perm())
	if err != nil {
		return err
	}
	err = unix.IoctlFileClone(int(output.Fd()), int(input.Fd()))
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EXDEV) || errors.Is(err, unix.ENOTTY) || errors.Is(err, unix.EINVAL) {
		_, err = io.Copy(output, input)
	}
	if err == nil {
		err = output.Chmod(info.Mode().Perm())
	}
	if err == nil {
		err = root.Chtimes(destination, info.ModTime(), info.ModTime())
	}
	return errors.Join(err, output.Close())
}

func copyTree(source string, destination *os.Root, skipGit bool) error {
	var directories []struct {
		path string
		mode os.FileMode
	}
	err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if skipGit && rel == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := rel
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			mode := info.Mode() & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
			directories = append(directories, struct {
				path string
				mode os.FileMode
			}{target, mode})
			return destination.MkdirAll(target, mode|0700)
		case entry.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return destination.Symlink(link, target)
		case entry.Type().IsRegular():
			return copyFile(path, target, destination)
		default:
			return fmt.Errorf("repository contains unsupported file %s (%s)", path, entry.Type())
		}
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := destination.Chmod(directories[i].path, directories[i].mode); err != nil {
			return err
		}
	}
	return nil
}

// validObjectID accepts the two object formats supported by Git. The source
// repository determines the actual format; Git validates referenced objects.
func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func publish(repo *repository) error {
	// Never run a host Git command against guest-controlled config or hooks.
	// Read HEAD, index and object files through a confined root, then register
	// the finished directory through the original repository's Git metadata.
	privateGit := filepath.Join(repo.Directory, ".git")
	box, err := os.OpenRoot(filepath.Dir(repo.CommonDirectory))
	if err != nil {
		return err
	}
	defer box.Close()
	common, err := box.OpenRoot(filepath.Base(repo.CommonDirectory))
	if err != nil {
		return err
	}
	defer common.Close()
	adminPath := filepath.Join("worktrees", filepath.Base(filepath.Dir(repo.Directory)))
	root, err := common.OpenRoot(adminPath)
	if err != nil {
		return err
	}
	defer root.Close()
	worktreePath, err := filepath.Rel(filepath.Dir(repo.CommonDirectory), repo.Directory)
	if err != nil {
		return err
	}
	info, err := box.Lstat(filepath.Join(worktreePath, ".git"))
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("private .git is not a regular file; repository retained")
	}
	gitfile, err := box.ReadFile(filepath.Join(worktreePath, ".git"))
	if err != nil {
		return err
	}
	forward := filepath.Join(repo.GuestRoot, filepath.Base(repo.CommonDirectory), adminPath)
	if string(gitfile) != "gitdir: "+forward+"\n" {
		return errors.New("private .git reference changed; repository retained")
	}
	head, err := root.ReadFile("HEAD")
	if err != nil {
		return err
	}
	commit := strings.TrimSpace(string(head))
	for depth := 0; strings.HasPrefix(commit, "ref: "); depth++ {
		if depth >= 8 {
			return errors.New("cyclic Git HEAD")
		}
		ref := strings.TrimPrefix(commit, "ref: ")
		if !strings.HasPrefix(ref, "refs/") || filepath.Clean(ref) != ref {
			return errors.New("invalid Git HEAD reference")
		}
		refs := common
		if within(ref, "refs/bisect") || within(ref, "refs/worktree") || within(ref, "refs/rewritten") {
			refs = root
		}
		value, readErr := refs.ReadFile(ref)
		if readErr == nil {
			commit = strings.TrimSpace(string(value))
			continue
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		packed, readErr := common.ReadFile("packed-refs")
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		commit = ""
		scanner := bufio.NewScanner(strings.NewReader(string(packed)))
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) == 2 && fields[1] == ref {
				commit = fields[0]
				break
			}
		}
		if err := scanner.Err(); err != nil {
			return err
		}
		if commit == "" {
			return errors.New("repository has no commit yet; standalone result retained")
		}
	}
	if !validObjectID(commit) {
		return errors.New("invalid Git HEAD object ID")
	}
	objects, err := git(repo.Source, "rev-parse", "--path-format=absolute", "--git-path", "objects")
	if err != nil {
		return err
	}
	if !repo.skipObjects {
		entries, err := fs.ReadDir(common.FS(), "objects")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			name := entry.Name()
			if name == "info" {
				continue
			} // Never import alternates or configuration.
			if !entry.IsDir() {
				return fmt.Errorf("unexpected object database entry %q", name)
			}
			if name != "pack" {
				if len(name) != 2 {
					return fmt.Errorf("unexpected object directory %q", name)
				}
				if _, err := hex.DecodeString(name); err != nil {
					return err
				}
			}
			children, err := fs.ReadDir(common.FS(), "objects/"+name)
			if err != nil {
				return err
			}
			for _, child := range children {
				filename := child.Name()
				if name == "pack" {
					stem, ext := strings.TrimSuffix(filename, filepath.Ext(filename)), filepath.Ext(filename)
					if (ext != ".pack" && ext != ".idx" && ext != ".rev") || !strings.HasPrefix(stem, "pack-") || !validObjectID(strings.TrimPrefix(stem, "pack-")) {
						continue
					}
				} else if !validObjectID(name + filename) {
					return fmt.Errorf("invalid loose object %q", filename)
				}
				if !child.Type().IsRegular() {
					return errors.New("non-regular Git object")
				}
				rel := "objects/" + name + "/" + filename
				input, err := common.Open(rel)
				if err != nil {
					return err
				}
				target := filepath.Join(objects, name, filename)
				if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
					input.Close()
					return err
				}
				output, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0444)
				if errors.Is(err, os.ErrExist) {
					input.Close()
					continue
				}
				if err != nil {
					input.Close()
					return err
				}
				_, err = io.Copy(output, input)
				err = errors.Join(err, input.Close(), output.Close())
				if err != nil {
					return err
				}
			}
		}
	}
	registration, err := os.MkdirTemp(filepath.Dir(repo.Directory), "register-")
	if err != nil {
		return err
	}
	if err := os.Remove(registration); err != nil {
		return err
	}
	if _, err := git(repo.Source, "worktree", "add", "--detach", "--no-checkout", registration, commit); err != nil {
		return err
	}
	gitfile, err = os.ReadFile(filepath.Join(registration, ".git"))
	if err != nil {
		return err
	}
	gitdir := strings.TrimSpace(strings.TrimPrefix(string(gitfile), "gitdir: "))
	index, err := root.ReadFile("index")
	if err == nil {
		err = os.WriteFile(filepath.Join(gitdir, "index"), index, 0644)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Shared split-index files contain data only; preserve them alongside index.
	files, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	for _, file := range files {
		if strings.HasPrefix(file.Name(), "sharedindex.") && validObjectID(strings.TrimPrefix(file.Name(), "sharedindex.")) {
			data, err := root.ReadFile(file.Name())
			if err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(gitdir, file.Name()), data, 0644); err != nil {
				return err
			}
		}
	}
	if err := os.Rename(privateGit, repo.Directory+".git"); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(registration, ".git"), privateGit); err != nil {
		return err
	}
	if _, err := git(repo.Source, "worktree", "repair", repo.Directory); err != nil {
		return err
	}
	return os.Remove(registration)
}
