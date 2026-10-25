//go:build linux && (amd64 || arm64) && successbox_mmap_probe

package observe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/usermem"
)

func init() { inspectSyscall = inspectMemoryProbe }

// Test policy only. The ordinary successbox platform never rewrites paths or
// payloads. Reserved getpid arguments coordinate checks with testdata/memory-guest.
func inspectMemoryProbe(m *syscallMemory, ac *arch.Context64) error {
	args := ac.SyscallArgs()
	switch ac.SyscallNo() {
	case unix.SYS_OPENAT:
		const original = "/tmp/successbox-mmap/original"
		const replacement = "/tmp/successbox-mmap/longer-replacement"
		if args[2].Int()&unix.O_ACCMODE != unix.O_RDONLY {
			return nil
		}
		name, err := m.task.CopyInString(args[1].Pointer(), len(original)+1)
		if err != nil || name != original {
			return nil
		}
		before, _, err := m.mmap(args[1].Pointer(), uint64(len(original)+1))
		if err != nil || string(before) != original+"\x00" {
			return fmt.Errorf("pathname source: %v", err)
		}
		data, address, err := m.mmap(0, uint64(len(replacement)+1))
		if err != nil {
			return err
		}
		if !bytes.Equal(data, make([]byte, len(data))) || len(data) != cap(data) {
			return fmt.Errorf("anonymous mapping was not zeroed or bounded")
		}
		copy(data, replacement+"\x00")
		visible, _, err := m.mmap(address, uint64(len(data)))
		if err != nil || !bytes.Equal(visible, data) {
			return fmt.Errorf("mapping changes are not guest-visible: %v", err)
		}
		args[1].Value = uintptr(address)
		setProbeArgs(ac, args)
		log.Debugf("successbox-mmap: longer pathname replacement")
	case unix.SYS_WRITEV:
		if args[2].Uint64() != 1 {
			return nil
		}
		// Probe only the fixture's payloads; leave unrelated writes untouched.
		var header [16]byte
		if _, err := m.mm.CopyIn(m.ctx, args[1].Pointer(), header[:], usermem.IOOpts{}); err != nil {
			return nil
		}
		length := binary.NativeEndian.Uint64(header[8:])
		if length != 13 && length != hostarch.PageSize+1 {
			return nil
		}
		source := hostarch.Addr(binary.NativeEndian.Uint64(header[:8]))
		var prefix [13]byte
		if _, err := m.mm.CopyIn(m.ctx, source, prefix[:], usermem.IOOpts{}); err != nil {
			return nil
		}
		if length == 13 && string(prefix[:]) != "initial input" || length != 13 && !bytes.Equal(prefix[:], bytes.Repeat([]byte{'x'}, len(prefix))) {
			return nil
		}
		vector, vectorAddress, err := m.mmap(args[1].Pointer(), 16)
		if err != nil {
			return err
		}
		data, address, err := m.mmap(source, length)
		if err != nil {
			return err
		}
		if length == 13 {
			copy(data, "edited output")
		} else {
			const suffix = " additional bytes"
			original := data
			data, address, err = m.mmap(0, length+uint64(len(suffix)))
			if err != nil {
				return err
			}
			copy(data, original)
			data[0], data[length-1] = 'a', 'z'
			copy(data[length:], suffix)
		}
		binary.NativeEndian.PutUint64(vector[:8], uint64(address))
		binary.NativeEndian.PutUint64(vector[8:], uint64(len(data)))
		args[1].Value = uintptr(vectorAddress)
		setProbeArgs(ac, args)
	case unix.SYS_GETPID:
		var result uint64
		switch args[0].Uint64() {
		case 0x53584201:
			data, address, err := m.mmap(0, 2*hostarch.PageSize+17)
			if err != nil {
				return err
			}
			if !bytes.Equal(data, make([]byte, len(data))) || len(data) != cap(data) {
				return fmt.Errorf("anonymous allocation was not zeroed or bounded")
			}
			data[0], data[len(data)-1] = 'A', 'Z'
			visible := make([]byte, len(data))
			if _, err := m.mm.CopyIn(m.ctx, address, visible, usermem.IOOpts{}); err != nil || !bytes.Equal(data, visible) {
				return fmt.Errorf("anonymous alias is not guest-visible: %v", err)
			}
			result = uint64(address)
		case 0x53584202, 0x53584203, 0x53584204:
			data, _, err := m.mmap(args[1].Pointer(), args[2].Uint64())
			if !errors.Is(err, linuxerr.EFAULT) && !errors.Is(err, unix.EFAULT) {
				return fmt.Errorf("expected source EFAULT, got %v", err)
			}
			if args[0].Uint64() == 0x53584203 {
				if string(data) != "prefix!!" {
					return fmt.Errorf("partial read lost its prefix")
				}
			} else if len(data) != 0 {
				return fmt.Errorf("invalid source produced data")
			}
			result = 1
		case 0x53584205:
			memoryCalls.Lock()
			count := len(memoryCalls.live)
			memoryCalls.Unlock()
			result = uint64(count)
		case 0x53584206:
			if _, _, err := m.mmap(0, 64); err != nil {
				return err
			}
			return fmt.Errorf("intentional mmap probe inspection failure")
		default:
			return nil
		}
		var output [8]byte
		binary.NativeEndian.PutUint64(output[:], result)
		_, err := m.mm.CopyOut(m.ctx, args[3].Pointer(), output[:], usermem.IOOpts{})
		return err
	case unix.SYS_NANOSLEEP:
		var duration [16]byte
		if _, err := m.mm.CopyIn(m.ctx, args[0].Pointer(), duration[:], usermem.IOOpts{}); err != nil {
			return nil
		}
		if binary.NativeEndian.Uint64(duration[:8]) != 0 || binary.NativeEndian.Uint64(duration[8:]) != 500000000 {
			return nil
		}
		_, address, err := m.mmap(args[0].Pointer(), 16)
		if err != nil {
			return err
		}
		args[0].Value = uintptr(address)
		setProbeArgs(ac, args)
		log.Debugf("successbox-mmap: restartable sleep input")
	case unix.SYS_EXECVE:
		name, err := m.task.CopyInString(args[0].Pointer(), 32)
		if err != nil || name != "/bin/true" {
			return nil
		}
		_, address, err := m.mmap(args[0].Pointer(), uint64(len(name)+1))
		if err != nil {
			return err
		}
		args[0].Value = uintptr(address)
		setProbeArgs(ac, args)
		log.Debugf("successbox-mmap: execve pathname copy")
	}
	return nil
}
