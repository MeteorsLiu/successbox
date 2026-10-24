//go:build linux && (amd64 || arm64)

package observe

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/gofer"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/unet"
)

// OpenDevice uses runsc's existing FD donation path. This socket belongs to
// the platform, not to any guest FDTable, and cannot be opened by guest code.
func (c *constructor) OpenDevice(devicePath string) (*fd.FD, error) {
	if devicePath == "" {
		return c.Constructor.OpenDevice(devicePath)
	}
	number, err := unix.Socket(unix.AF_UNIX, unix.SOCK_SEQPACKET|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if err := unix.Connect(number, &unix.SockaddrUnix{Name: devicePath}); err != nil {
		unix.Close(number)
		return nil, err
	}
	return fd.New(number), nil
}

type worktreeMounts struct {
	mu      sync.Mutex
	socket  *unet.Socket
	mounted map[*vfs.MountNamespace]map[string]bool
}

type pathArgument struct {
	dirfd   int32
	address int
}

// Names come from the registered architecture's syscall table. This keeps
// legacy amd64 calls and the *at-only arm64 ABI on the same policy path.
func pathnameArguments(name string, args arch.SyscallArguments) []pathArgument {
	cwd := int32(unix.AT_FDCWD)
	switch name {
	case "open", "creat", "stat", "lstat", "access", "readlink", "chdir", "execve", "unlink", "rmdir", "mkdir", "truncate", "chmod", "chown", "lchown", "utime", "utimes", "statfs", "setxattr", "lsetxattr", "getxattr", "lgetxattr", "listxattr", "llistxattr", "removexattr", "lremovexattr", "mknod":
		return []pathArgument{{cwd, 0}}
	case "openat", "openat2", "newfstatat", "fstatat64", "statx", "faccessat", "faccessat2", "readlinkat", "execveat", "unlinkat", "mkdirat", "fchmodat", "fchmodat2", "fchownat", "utimensat", "futimesat", "mknodat":
		return []pathArgument{{args[0].Int(), 1}}
	case "rename", "link":
		return []pathArgument{{cwd, 0}, {cwd, 1}}
	case "renameat", "renameat2", "linkat":
		return []pathArgument{{args[0].Int(), 1}, {args[2].Int(), 3}}
	case "symlink":
		return []pathArgument{{cwd, 1}}
	case "symlinkat":
		return []pathArgument{{args[1].Int(), 2}}
	}
	return nil
}

func (w *worktreeMounts) inspect(t *kernel.Task, ac *arch.Context64, initial bool) error {
	args := ac.SyscallArgs()
	syscall := t.SyscallTable().Table[ac.SyscallNo()]
	arguments := pathnameArguments(syscall.Name, args)
	if !initial && len(arguments) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	root := t.FSContext().RootDirectory()
	defer root.DecRef(t)
	ctx := vfs.WithRoot(vfs.WithMountNamespace(t.Kernel().SupervisorContext(), t.MountNamespace()), root)
	vfsObj := t.Kernel().VFS()
	cwd := t.FSContext().WorkingDirectory()
	cwdPath, err := vfsObj.PathnameForGetcwd(ctx, root, cwd)
	defer cwd.DecRef(ctx)
	if err != nil {
		return nil
	} // Preserve the original syscall's deleted-cwd behavior.
	if initial {
		if err := w.ensure(ctx, t, root, cwdPath); err != nil {
			return err
		}
	}
	for _, argument := range arguments {
		filename, err := t.CopyInString(args[argument.address].Pointer(), unix.PathMax)
		if err != nil || filename == "" {
			continue
		} // Sentry supplies EFAULT/ENOENT/AT_EMPTY_PATH.
		start := root
		if !strings.HasPrefix(filename, "/") {
			if argument.dirfd == unix.AT_FDCWD {
				start = t.FSContext().WorkingDirectory()
			} else {
				file, _ := t.FDTable().Get(argument.dirfd)
				if file == nil {
					continue
				}
				start = file.VirtualDentry()
				start.IncRef()
				file.DecRef(ctx)
			}
		} else {
			start.IncRef()
		}
		// Resolve the parent as well as the final target. For /repo/link/file,
		// this discovers a repository containing the link and one at its target.
		// Do not Clean before resolution: symlink/.. is not lexical parenthood.
		names := []string{filename}
		if slash := strings.LastIndex(strings.TrimRight(filename, "/"), "/"); slash >= 0 {
			parent := filename[:slash]
			if parent == "" {
				parent = "/"
			}
			names = append([]string{parent}, names...)
		}
		for _, name := range names {
			for {
				vd, lookupErr := vfsObj.GetDentryAt(ctx, t.Credentials(), &vfs.PathOperation{Root: root, Start: start, Path: fspath.Parse(name), FollowFinalSymlink: true}, &vfs.GetDentryOptions{})
				if lookupErr == nil {
					resolved, lookupErr := vfsObj.PathnameWithDeleted(ctx, root, vd)
					vd.DecRef(ctx)
					if lookupErr == nil {
						if err := w.ensure(ctx, t, root, resolved); err != nil {
							start.DecRef(ctx)
							return err
						}
					}
					break
				}
				if !linuxerr.Equals(linuxerr.ENOENT, lookupErr) && !linuxerr.Equals(linuxerr.ENOTDIR, lookupErr) {
					break
				}
				slash := strings.LastIndex(strings.TrimRight(name, "/"), "/")
				if slash < 0 {
					break
				}
				name = name[:slash]
				if name == "" {
					name = "/"
				}
				if name == "/" {
					break
				}
			}
		}
		start.DecRef(ctx)
	}
	// Only the bootstrap cwd can refer to a directory from before discovery.
	// Do not reset cwd on every syscall: another thread may be doing chdir.
	if initial {
		vd, err := vfsObj.GetDentryAt(ctx, t.Credentials(), &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(cwdPath), FollowFinalSymlink: true}, &vfs.GetDentryOptions{})
		if err == nil {
			if vd != cwd {
				current := t.FSContext().WorkingDirectory()
				if current == cwd {
					t.FSContext().SetWorkingDirectory(ctx, vd)
				}
				current.DecRef(ctx)
			}
			vd.DecRef(ctx)
		}
	}
	return nil
}

func (w *worktreeMounts) ensure(ctx context.Context, t *kernel.Task, root vfs.VirtualDentry, filename string) error {
	mounts := w.mounted[t.MountNamespace()]
	for directory := range mounts {
		if filename == directory || strings.HasPrefix(filename, directory+"/") {
			return nil
		}
	}
	creds := t.Credentials()
	groups := make([]uint32, len(creds.ExtraKGIDs))
	for i, id := range creds.ExtraKGIDs {
		groups[i] = uint32(id)
	}
	req := struct {
		Path     string
		UID, GID uint32
		Groups   []uint32
	}{filename, uint32(creds.EffectiveKUID), uint32(creds.EffectiveKGID), groups}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if n, err := w.socket.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	buffer := make([]byte, 64*1024)
	reader := w.socket.Reader(true)
	reader.EnableFDs(1)
	n, err := reader.ReadVec([][]byte{buffer})
	if err != nil {
		reader.CloseFDs()
		return err
	}
	fds, err := reader.ExtractFDs()
	if err != nil {
		return err
	}
	defer func() {
		for _, number := range fds {
			unix.Close(number)
		}
	}()
	if n == 0 {
		return io.EOF
	}
	if n == len(buffer) {
		return errors.New("oversized worktree response")
	}
	var res struct{ Path, Error string }
	if err := json.Unmarshal(buffer[:n], &res); err != nil {
		return err
	}
	if res.Error != "" {
		return fmt.Errorf("worktree: %s", res.Error)
	}
	if res.Path == "" {
		return nil
	}
	if len(fds) != 1 || !path.IsAbs(res.Path) {
		return errors.New("invalid worktree mount response")
	}
	opts := vfs.MountOptions{Locked: true, Flags: vfs.MountFlags{NoSUID: true}, GetFilesystemOptions: vfs.GetFilesystemOptions{InternalMount: true, Data: fmt.Sprintf("trans=fd,rfdno=%d,wfdno=%d,directfs,disable_fifo_open", fds[0], fds[0])}}
	// The gofer filesystem takes ownership of the donated socket, including
	// failed mount initialization. No descriptor is installed into the guest.
	fds = nil
	if _, err := t.Kernel().VFS().MountAt(ctx, auth.NewRootCredentials(t.Kernel().RootUserNamespace()), "successbox-worktree", &vfs.PathOperation{Root: root, Start: root, Path: fspath.Parse(res.Path), FollowFinalSymlink: true}, gofer.Name, &opts); err != nil {
		return fmt.Errorf("mount worktree %s: %w", res.Path, err)
	}
	if mounts == nil {
		mounts = make(map[string]bool)
		w.mounted[t.MountNamespace()] = mounts
	}
	mounts[res.Path] = true
	log.Infof("successbox-worktree: mounted %s", res.Path)
	return nil
}
