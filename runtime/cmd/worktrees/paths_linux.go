//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"log"
	"path/filepath"
	"strings"

	"github.com/MeteorsLiu/successbox/runtime/internal/intercept"
	"golang.org/x/sys/unix"
)

type pathArgument struct {
	dirfd   int32
	address int
}

// Names come from the architecture's syscall table, so the policy does not
// depend on amd64's legacy syscall numbers or arm64's *at-only ABI.
func pathnameArguments(name string, args [6]uint64) []pathArgument {
	cwd := int32(unix.AT_FDCWD)
	switch name {
	case "open", "creat", "stat", "lstat", "access", "readlink", "chdir", "execve", "unlink", "rmdir", "mkdir", "truncate", "chmod", "chown", "lchown", "utime", "utimes", "statfs", "setxattr", "lsetxattr", "getxattr", "lgetxattr", "listxattr", "llistxattr", "removexattr", "lremovexattr", "mknod":
		return []pathArgument{{cwd, 0}}
	case "openat", "openat2", "newfstatat", "fstatat64", "fstatat", "statx", "faccessat", "faccessat2", "readlinkat", "execveat", "unlinkat", "mkdirat", "fchmodat", "fchmodat2", "fchownat", "utimensat", "futimesat", "mknodat":
		return []pathArgument{{int32(args[0]), 1}}
	case "rename", "link":
		return []pathArgument{{cwd, 0}, {cwd, 1}}
	case "renameat", "renameat2", "linkat":
		return []pathArgument{{int32(args[0]), 1}, {int32(args[2]), 3}}
	case "symlink":
		return []pathArgument{{cwd, 1}}
	case "symlinkat":
		return []pathArgument{{int32(args[1]), 2}}
	}
	return nil
}

func (m *manager) original(path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for source, private := range m.mappings {
		if within(path, private) {
			return source + strings.TrimPrefix(path, private)
		}
	}
	return path
}

// A nested guest mount takes precedence over a repository mapping. In
// particular, /repo/readonly must not become writable through /private/repo.
func (m *manager) mapped(path string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	boundary := "/"
	writable := m.root != ""
	for _, mount := range m.mounts {
		target := filepath.Clean(mount.Destination)
		if within(path, target) && len(target) > len(boundary) {
			boundary = target
			writable = mount.Type == "bind"
			for _, option := range mount.Options {
				if option == "ro" {
					writable = false
				}
			}
		}
	}
	if !writable {
		return ""
	}
	for source, private := range m.mappings {
		if within(source, boundary) && within(path, source) {
			return private + strings.TrimPrefix(path, source)
		}
	}
	return ""
}

// Resolve one component at a time so that /repo/link/../file follows link before
// applying .., and absolute symlinks still enter the right private repository.
func (m *manager) resolve(c *syscallEvent, dirfd int32, path string, follow bool) (string, error) {
	current := "/"
	if !strings.HasPrefix(path, "/") {
		cwd, err := c.resolvePath(dirfd, ".", true)
		if err != nil {
			return "", err
		}
		current = m.original(cwd.Path)
	}
	parts := strings.Split(path, "/")
	links := 0
	redirected := false
	for len(parts) > 0 {
		part := parts[0]
		parts = parts[1:]
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			current = filepath.Dir(current)
			continue
		}
		candidate := filepath.Join(current, part)
		req := request{UID: c.uid, GID: c.gid, Groups: c.groups}
		req.Path = candidate
		// Check the original component before discovery: a symlink named
		// /alias/.git does not make /alias a separate source repository.
		mapped := m.mapped(candidate)
		var result intercept.Message
		var err error
		if mapped == "" {
			result, err = c.resolvePath(unix.AT_FDCWD, candidate, false)
			if err == nil && result.Link == nil {
				mapped, err = m.prepare(req)
				if err != nil {
					log.Printf("worktree path failure: syscall=%s path=%q uid=%d: %v", c.Name, path, c.uid, err)
					return "", err
				}
			}
		}
		if mapped == "" {
			mapped = candidate
		} else {
			redirected = true
		}
		if redirected {
			result, err = c.resolvePath(unix.AT_FDCWD, mapped, false)
		}

		if errors.Is(err, unix.ENOENT) && len(parts) == 0 {
			if strings.HasSuffix(path, "/") {
				mapped += "/"
			}
			if !redirected {
				return path, nil
			}
			return mapped, nil
		}
		if err != nil {
			if !redirected {
				return path, nil
			}
			return "", err
		}
		if result.Link != nil && (follow || len(parts) > 0) {
			links++
			if links > 40 {
				if !redirected {
					return path, nil
				}
				return "", unix.ELOOP
			}
			target := *result.Link
			if strings.HasPrefix(target, "/") {
				current = "/"
			}
			parts = append(strings.Split(target, "/"), parts...)
			continue
		}
		if len(parts) > 0 && result.Mode&unix.S_IFMT != unix.S_IFDIR {
			if !redirected {
				return path, nil
			}
			return "", unix.ENOTDIR
		}
		current = m.original(result.Path)
	}
	mapped := m.mapped(current)
	if mapped == "" {
		mapped = current
	} else {
		redirected = true
	}
	if !redirected {
		return path, nil
	}
	if strings.HasSuffix(path, "/") && mapped != "/" {
		mapped += "/"
	}
	return mapped, nil
}

func (m *manager) inspect(c *syscallEvent) error {
	paths := pathnameArguments(c.Name, c.Args)
	if !c.First && len(paths) == 0 && c.Name != "getcwd" {
		return nil
	}
	// The initial cwd is the only directory handle established before the first
	// callback. Later opens/chdir use rewritten arguments through normal Sentry.
	if c.First {
		cwd, err := c.resolvePath(unix.AT_FDCWD, ".", true)
		if err == nil {
			req := request{UID: c.uid, GID: c.gid, Groups: c.groups}
			req.Path = m.original(cwd.Path)
			private, err := m.prepare(req)
			if err != nil {
				log.Printf("worktree initial directory failure: cwd=%q uid=%d: %v", req.Path, c.uid, err)
				return err
			}
			if private != "" && private != cwd.Path {
				if err := c.setWorkingDirectory(private); err != nil {
					return err
				}
			}
		}
	}
	if c.Name == "getcwd" {
		cwd, err := c.resolvePath(unix.AT_FDCWD, ".", true)
		if err != nil {
			return nil
		}
		original := m.original(cwd.Path)
		if original == cwd.Path {
			return nil
		}
		data := []byte(original + "\x00")
		if uint64(len(data)) > c.Args[1] {
			c.Result = &intercept.Result{Errno: uint32(unix.ERANGE)}
			return nil
		}
		if err := c.copyOut(c.Args[0], data); err != nil {
			var errno unix.Errno
			if errors.As(err, &errno) {
				c.Result = &intercept.Result{Errno: uint32(errno)}
				return nil
			}
			return err
		}
		c.Result = &intercept.Result{Value: uint64(len(data))}
		return nil
	}
	for _, argument := range paths {
		path, err := c.readString(c.Args[argument.address])
		if err != nil || path == "" {
			continue
		}
		follow := true
		var resolveFlags uint64
		switch c.Name {
		case "lstat", "readlink", "readlinkat", "unlink", "unlinkat", "rmdir", "mkdir", "mkdirat", "rename", "renameat", "renameat2", "link", "symlink", "symlinkat", "lchown", "lsetxattr", "lgetxattr", "llistxattr", "lremovexattr", "mknod", "mknodat":
			follow = false
		case "openat2":
			if c.Args[3] < 24 || c.Args[3] > uint64(unix.Getpagesize()) {
				continue
			}
			how, err := c.mmap(c.Args[2], int(c.Args[3]))
			if err != nil {
				continue
			}
			flags := binary.NativeEndian.Uint64(how.Data[:8])
			resolveFlags = binary.NativeEndian.Uint64(how.Data[16:24])
			follow = flags&unix.O_NOFOLLOW == 0 && flags&(unix.O_CREAT|unix.O_EXCL) != (unix.O_CREAT|unix.O_EXCL)
		case "open", "openat":
			flags := c.Args[argument.address+1]
			follow = flags&unix.O_NOFOLLOW == 0 && flags&(unix.O_CREAT|unix.O_EXCL) != (unix.O_CREAT|unix.O_EXCL)
		case "newfstatat", "fstatat64", "fstatat", "fchownat", "faccessat2", "utimensat":
			index := 3
			if c.Name == "fchownat" {
				index = 4
			}
			follow = c.Args[index]&unix.AT_SYMLINK_NOFOLLOW == 0
		case "statx":
			follow = c.Args[2]&unix.AT_SYMLINK_NOFOLLOW == 0
		case "execveat":
			follow = c.Args[4]&unix.AT_SYMLINK_NOFOLLOW == 0
		case "linkat":
			follow = argument.address == 1 && c.Args[4]&unix.AT_SYMLINK_FOLLOW != 0
		}
		rewritten, err := m.resolve(c, argument.dirfd, path, follow)
		if err != nil {
			var errno unix.Errno
			if errors.As(err, &errno) {
				c.Result = &intercept.Result{Errno: uint32(errno)}
				return nil
			}
			return err
		}
		if rewritten == path {
			continue
		}
		// Absolute remapping cannot preserve openat2's directory confinement
		// and no-symlink/no-mount constraints. Do not silently discard them.
		if resolveFlags != 0 {
			c.Result = &intercept.Result{Errno: uint32(unix.EOPNOTSUPP)}
			return nil
		}
		memory, err := c.mmap(0, len(rewritten)+1)
		if err != nil {
			return err
		}
		copy(memory.Data, rewritten+"\x00")
		c.Args[argument.address] = memory.Addr
	}
	return nil
}
