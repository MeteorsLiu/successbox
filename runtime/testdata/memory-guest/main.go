//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func probe(marker, address, size uintptr) uint64 {
	var result uint64
	_, _, errno := unix.RawSyscall6(unix.SYS_GETPID, marker, address, size, uintptr(unsafe.Pointer(&result)), 0, 0)
	if errno != 0 {
		panic(errno)
	}
	return result
}

func writeInput(input, expected []byte) error {
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		return err
	}
	defer unix.Close(pipe[0])
	defer unix.Close(pipe[1])
	original := bytes.Clone(input)
	n, err := unix.Writev(pipe[1], [][]byte{input})
	if err != nil || n != len(expected) {
		return fmt.Errorf("writev: bytes=%d, want=%d, error=%v", n, len(expected), err)
	}
	if !bytes.Equal(input, original) {
		return fmt.Errorf("writev changed the original guest buffer")
	}
	output := make([]byte, len(expected))
	n, err = unix.Read(pipe[0], output)
	if err != nil || n != len(expected) || !bytes.Equal(output, expected) {
		return fmt.Errorf("writev content: bytes=%d, error=%v", n, err)
	}
	runtime.KeepAlive(input)
	return nil
}

func run() error {
	if len(os.Args) == 2 {
		switch os.Args[1] {
		case "failure":
			probe(0x53584206, 0, 0)
			return fmt.Errorf("failed inspector resumed the guest")
		case "exec":
			return unix.Exec("/bin/true", []string{"/bin/true"}, os.Environ())
		case "restart":
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			signals := make(chan os.Signal, 1)
			signal.Notify(signals, unix.SIGUSR1)
			defer signal.Stop(signals)
			tid := unix.Gettid()
			sent := make(chan error, 1)
			go func() {
				time.Sleep(40 * time.Millisecond)
				sent <- unix.Tgkill(unix.Getpid(), tid, unix.SIGUSR1)
			}()
			duration := unix.Timespec{Nsec: 500000000}
			var remaining unix.Timespec
			err := unix.Nanosleep(&duration, &remaining)
			if signalErr := <-sent; signalErr != nil {
				return signalErr
			}
			if err != unix.EINTR || probe(0x53584205, 0, 0) == 0 {
				return fmt.Errorf("restartable call did not retain its mapping: %v", err)
			}
			fmt.Println("restart retention passed")
			return nil
		case "clean":
			if count := probe(0x53584205, 0, 0); count != 0 {
				return fmt.Errorf("%d mappings remain live", count)
			}
			fmt.Println("memory clean")
			return nil
		}
	}
	page := unix.Getpagesize()
	address := probe(0x53584201, 0, 0)
	if address == 0 {
		return fmt.Errorf("anonymous MMap probe did not run")
	}
	var residency byte
	_, _, errno := unix.RawSyscall(unix.SYS_MINCORE, uintptr(address), uintptr(page), uintptr(unsafe.Pointer(&residency)))
	if errno != unix.ENOMEM {
		return fmt.Errorf("temporary mapping survived syscall completion: %v", errno)
	}
	if err := os.MkdirAll("/tmp/successbox-mmap", 0700); err != nil {
		return err
	}
	const original = "/tmp/successbox-mmap/original"
	const replacement = "/tmp/successbox-mmap/longer-replacement"
	if err := os.WriteFile(original, []byte("original content"), 0600); err != nil {
		return err
	}
	if err := os.WriteFile(replacement, []byte("replacement content"), 0600); err != nil {
		return err
	}
	name := append([]byte(original), 0)
	dirfd := int(unix.AT_FDCWD)
	fd, _, errno := unix.Syscall6(unix.SYS_OPENAT, uintptr(dirfd), uintptr(unsafe.Pointer(&name[0])), unix.O_RDONLY|unix.O_CLOEXEC, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	output := make([]byte, 64)
	n, err := unix.Read(int(fd), output)
	_ = unix.Close(int(fd))
	if err != nil || string(output[:n]) != "replacement content" || string(name) != original+"\x00" {
		return fmt.Errorf("pathname replacement failed: %q, %v", output[:n], err)
	}
	runtime.KeepAlive(name)
	var calls sync.WaitGroup
	errors := make(chan error, 8)
	for range 8 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			for range 128 {
				if err := writeInput([]byte("initial input"), []byte("edited output")); err != nil {
					errors <- err
					return
				}
			}
		}()
	}
	calls.Wait()
	close(errors)
	for err := range errors {
		return err
	}
	large := bytes.Repeat([]byte{'x'}, page+1)
	want := append(bytes.Clone(large), []byte(" additional bytes")...)
	want[0], want[len(large)-1] = 'a', 'z'
	if err := writeInput(large, want); err != nil {
		return err
	}
	dead, err := unix.Mmap(-1, 0, page, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return err
	}
	deadAddress := uintptr(unsafe.Pointer(&dead[0]))
	if err := unix.Munmap(dead); err != nil {
		return err
	}
	if probe(0x53584202, deadAddress, uintptr(page)) != 1 {
		return fmt.Errorf("unmapped source check did not run")
	}
	guarded, err := unix.Mmap(-1, 0, 2*page, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_PRIVATE|unix.MAP_ANONYMOUS)
	if err != nil {
		return err
	}
	defer unix.Munmap(guarded)
	copy(guarded[page-8:], "prefix!!")
	if err := unix.Mprotect(guarded[page:], unix.PROT_NONE); err != nil {
		return err
	}
	guardAddress := uintptr(unsafe.Pointer(&guarded[page]))
	if probe(0x53584203, guardAddress-8, 16) != 1 || probe(0x53584204, guardAddress, 8) != 1 {
		return fmt.Errorf("partial/protected source checks did not run")
	}
	runtime.KeepAlive(guarded)
	if count := probe(0x53584205, 0, 0); count != 0 {
		return fmt.Errorf("%d mappings remain after synchronous calls", count)
	}
	fmt.Println("mmap: zeroed allocation, guest aliases, longer pathname, nested writev, cross-page buffers, concurrent calls, read boundaries and cleanup passed")
	return nil
}
