//go:build linux && (amd64 || arm64)

package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/MeteorsLiu/successbox/runtime/internal/intercept"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	gerrors "gvisor.dev/gvisor/pkg/errors"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/unet"
	"gvisor.dev/gvisor/pkg/usermem"
)

// OpenDevice uses runsc's existing FD donation path. The connection belongs to
// the platform and is never installed into a guest FD table.
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

type remoteInspector struct {
	mu     sync.Mutex
	socket *unet.Socket
}

// One connection carries one synchronous callback at a time. The guest task
// resumes only after the caller has dropped all aliases and returned its args.
func (r *remoteInspector) inspect(m *syscallMemory, ac *arch.Context64, first bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	args := ac.SyscallArgs()
	event := intercept.Message{Op: "syscall", First: first, Name: m.task.SyscallTable().Table[ac.SyscallNo()].Name}
	creds := m.task.Credentials()
	event.UID, event.GID = uint32(creds.EffectiveKUID), uint32(creds.EffectiveKGID)
	for _, gid := range creds.ExtraKGIDs {
		event.Groups = append(event.Groups, uint32(gid))
	}
	for i, arg := range args {
		event.Args[i] = arg.Uint64()
	}
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if n, err := r.socket.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	buffer := make([]byte, 65536)
	for {
		n, err := r.socket.Read(buffer)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.EOF
		}
		if n == len(buffer) {
			return errors.New("oversized interceptor request")
		}
		var req intercept.Message
		if err := json.Unmarshal(buffer[:n], &req); err != nil {
			return err
		}
		if req.Op == "resume" {
			if req.Error != "" {
				return fmt.Errorf("syscall interceptor: %s", req.Error)
			}
			for i, value := range req.Args {
				args[i].Value = uintptr(value)
			}
			setSyscallArgs(ac, args)
			m.result = req.Result
			return nil
		}
		res, descriptors, operationErr := m.operation(req)
		if operationErr != nil {
			var errno *gerrors.Error
			var unixErr unix.Errno
			if errors.As(operationErr, &errno) {
				res.Errno = uint32(errno.Errno())
			} else if errors.As(operationErr, &unixErr) {
				res.Errno = uint32(unixErr)
			} else {
				res.Error = operationErr.Error()
			}
		}
		data, err := json.Marshal(res)
		if err != nil {
			return err
		}
		if len(data) >= len(buffer) || len(descriptors) > 253 {
			return errors.New("oversized interceptor response")
		}
		writer := r.socket.Writer(true)
		writer.PackFDs(descriptors...)
		n, err = writer.WriteVec([][]byte{data})
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
	}
}

func (m *syscallMemory) operation(req intercept.Message) (intercept.Message, []int, error) {
	var res intercept.Message
	switch req.Op {
	case "mmap":
		data, address, err := m.mmap(hostarch.Addr(req.Address), req.Size)
		res.Address, res.Size = uint64(address), uint64(len(data))
		if len(data) == 0 {
			return res, nil, err
		}
		region := m.regions[len(m.regions)-1]
		var descriptors []int
		for _, pin := range region.pins {
			number, fdErr := pin.File.DataFD(pin.FileRange())
			if fdErr != nil {
				return intercept.Message{}, nil, fdErr
			}
			res.Ranges = append(res.Ranges, intercept.Range{FD: len(descriptors), Offset: pin.Offset, Start: uint64(pin.Source.Start - region.address), Length: uint64(pin.Source.Length())})
			descriptors = append(descriptors, number)
		}
		// The ranges cover the full allocation even when copying input stopped at
		// a fault; Size exposes only the readable prefix to the callback.
		res.Length = region.length
		return res, descriptors, err
	case "string":
		value, err := m.task.CopyInString(hostarch.Addr(req.Address), unix.PathMax)
		res.Path = value
		return res, nil, err
	case "copyout":
		_, err := m.mm.CopyOut(m.ctx, hostarch.Addr(req.Address), req.Data, usermem.IOOpts{})
		return res, nil, err
	case "path", "cwd":
		return m.pathOperation(req)
	default:
		return res, nil, linuxerr.EINVAL
	}
}

func (m *syscallMemory) pathOperation(req intercept.Message) (intercept.Message, []int, error) {
	var res intercept.Message
	t := m.task
	root := t.FSContext().RootDirectory()
	defer root.DecRef(m.ctx)
	// Switch is still in application accounting state. VFS waits must use the
	// supervisor context, while lookup permissions use this task's credentials.
	ctx := vfs.WithRoot(vfs.WithMountNamespace(m.ctx, t.MountNamespace()), root)
	start := root
	if !strings.HasPrefix(req.Path, "/") {
		if req.DirFD == unix.AT_FDCWD {
			start = t.FSContext().WorkingDirectory()
		} else {
			file, _ := t.FDTable().Get(req.DirFD)
			if file == nil {
				return res, nil, linuxerr.EBADF
			}
			start = file.VirtualDentry()
			start.IncRef()
			file.DecRef(ctx)
		}
	} else {
		start.IncRef()
	}
	defer start.DecRef(ctx)
	pop := vfs.PathOperation{Root: root, Start: start, Path: fspath.Parse(req.Path), FollowFinalSymlink: req.Follow}
	vd, err := t.Kernel().VFS().GetDentryAt(ctx, t.Credentials(), &pop, &vfs.GetDentryOptions{CheckSearchable: req.Op == "cwd"})
	if err != nil {
		return res, nil, err
	}
	defer vd.DecRef(ctx)
	if req.Op == "cwd" {
		t.FSContext().SetWorkingDirectory(ctx, vd)
		return res, nil, nil
	}
	res.Path, err = t.Kernel().VFS().PathnameWithDeleted(ctx, root, vd)
	if err != nil {
		return res, nil, err
	}
	stat, err := t.Kernel().VFS().StatAt(ctx, t.Credentials(), &pop, &vfs.StatOptions{Mask: linux.STATX_TYPE})
	if err != nil {
		return res, nil, err
	}
	res.Mode = uint32(stat.Mode)
	if stat.Mode&linux.S_IFMT == linux.S_IFLNK {
		link, err := t.Kernel().VFS().ReadlinkAt(ctx, t.Credentials(), &pop)
		if err != nil {
			return res, nil, err
		}
		res.Link = &link
	}
	return res, nil, nil
}
