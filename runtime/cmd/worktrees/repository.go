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

func prepare(source, destination string) error {
	// --no-hardlinks keeps even Git objects independent of the source. Working
	// files use reflinks when the backing filesystem supports them, never links.
	if _, err := git("/", "clone", "--local", "--no-hardlinks", "--dissociate", "--no-checkout", "--", source, destination); err != nil {
		return err
	}
	if err := copyTree(source, destination, true); err != nil {
		return err
	}
	index, err := git(source, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return err
	}
	if _, err := os.Stat(index); err == nil {
		if err := copyFile(index, filepath.Join(destination, ".git", "index")); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Restore the user's repository configuration after preparation. No host
	// Git command is subsequently run against this guest-writable metadata.
	config, err := git(source, "rev-parse", "--path-format=absolute", "--git-path", "config")
	if err != nil {
		return err
	}
	if err := copyFile(config, filepath.Join(destination, ".git", "config")); err != nil {
		return err
	}
	shared, err := filepath.Glob(filepath.Join(filepath.Dir(index), "sharedindex.*"))
	if err != nil {
		return err
	}
	for _, file := range shared {
		if err := copyFile(file, filepath.Join(destination, ".git", filepath.Base(file))); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(source, destination string) error {
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
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|unix.O_NOFOLLOW, info.Mode().Perm())
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
		err = os.Chtimes(destination, info.ModTime(), info.ModTime())
	}
	return errors.Join(err, output.Close())
}

func copyTree(source, destination string, skipGit bool) error {
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
		target := filepath.Join(destination, rel)
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
			return os.MkdirAll(target, mode|0700)
		case entry.Type()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case entry.Type().IsRegular():
			return copyFile(path, target)
		default:
			return fmt.Errorf("repository contains unsupported file %s (%s)", path, entry.Type())
		}
	})
	if err != nil {
		return err
	}
	for i := len(directories) - 1; i >= 0; i-- {
		if err := os.Chmod(directories[i].path, directories[i].mode); err != nil {
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
	info, err := os.Lstat(privateGit)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("private .git is not a directory; repository retained")
	}
	root, err := os.OpenRoot(privateGit)
	if err != nil {
		return err
	}
	defer root.Close()
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
		value, readErr := root.ReadFile(ref)
		if readErr == nil {
			commit = strings.TrimSpace(string(value))
			continue
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		packed, readErr := root.ReadFile("packed-refs")
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
	entries, err := fs.ReadDir(root.FS(), "objects")
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
		children, err := fs.ReadDir(root.FS(), "objects/"+name)
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
			input, err := root.Open(rel)
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
	gitfile, err := os.ReadFile(filepath.Join(registration, ".git"))
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
