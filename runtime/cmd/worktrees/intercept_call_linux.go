package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"unsafe"

	"github.com/MeteorsLiu/successbox/runtime/internal/intercept"
	"golang.org/x/sys/unix"
)

// syscallEvent is borrowed by one synchronous callback. Args may be modified before
// returning. intercept.Result, when non-nil, suppresses dispatch of the original syscall.
type syscallEvent struct {
	Name     string
	Args     [6]uint64
	First    bool
	uid, gid uint32
	groups   []uint32
	Result   *intercept.Result
	mu       sync.Mutex
	conn     *net.UnixConn
	active   bool
	mappings [][]byte
}

// memory aliases temporary guest pages; it is valid only during the callback.
type memory struct {
	Data []byte
	Addr uint64
}

func (c *syscallEvent) request(req intercept.Message) (intercept.Message, []int, error) {
	if !c.active {
		return intercept.Message{}, nil, errors.New("interceptor callback has returned")
	}
	data, err := json.Marshal(req)
	if err != nil {
		return intercept.Message{}, nil, err
	}
	if len(data) >= 65536 {
		return intercept.Message{}, nil, errors.New("interceptor message too large")
	}
	n, err := c.conn.Write(data)
	if err != nil {
		return intercept.Message{}, nil, err
	}
	if n != len(data) {
		return intercept.Message{}, nil, io.ErrShortWrite
	}
	buffer := make([]byte, 65536)
	control := make([]byte, unix.CmsgSpace(253*4))
	n, oob, flags, _, err := c.conn.ReadMsgUnix(buffer, control)
	if err != nil {
		return intercept.Message{}, nil, err
	}
	var fds []int
	messages, err := unix.ParseSocketControlMessage(control[:oob])
	if err != nil {
		return intercept.Message{}, nil, err
	}
	for _, message := range messages {
		received, err := unix.ParseUnixRights(&message)
		if err != nil {
			for _, fd := range fds {
				unix.Close(fd)
			}
			return intercept.Message{}, nil, err
		}
		fds = append(fds, received...)
	}
	var res intercept.Message
	if n == 0 {
		err = io.EOF
	} else if flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 {
		err = errors.New("truncated interceptor response")
	} else {
		err = json.Unmarshal(buffer[:n], &res)
	}
	if err == nil && res.Error != "" {
		err = errors.New(res.Error)
	}
	if err != nil {
		for _, fd := range fds {
			unix.Close(fd)
		}
		return intercept.Message{}, nil, err
	}
	return res, fds, nil
}

// mmap allocates independent guest pages. A nonzero address copies guest input
// first. Data aliases the same pages in the host, not the original guest input.
func (c *syscallEvent) mmap(address uint64, size int) (memory, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if size < 0 {
		return memory{}, unix.EINVAL
	}
	res, fds, err := c.request(intercept.Message{Op: "mmap", Address: address, Size: uint64(size)})
	if err != nil {
		return memory{}, err
	}
	defer func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}()
	if res.Size == 0 {
		if res.Errno != 0 {
			return memory{}, unix.Errno(res.Errno)
		}
		return memory{}, nil
	}
	length := res.Length
	if length == 0 || length%uint64(unix.Getpagesize()) != 0 || res.Size > length {
		return memory{}, errors.New("invalid temporary memory length")
	}
	if length > uint64(^uint(0)>>1) {
		return memory{}, unix.EINVAL
	}
	mapping, err := unix.Mmap(-1, 0, int(length), unix.PROT_NONE, unix.MAP_PRIVATE|unix.MAP_ANON)
	if err != nil {
		return memory{}, err
	}
	var covered uint64
	for _, r := range res.Ranges {
		if r.FD < 0 || r.FD >= len(fds) || r.Start != covered || r.Length == 0 || r.Length%uint64(unix.Getpagesize()) != 0 || r.Length > length-covered || r.Offset%uint64(unix.Getpagesize()) != 0 {
			unix.Munmap(mapping)
			return memory{}, errors.New("invalid temporary memory range")
		}
		_, err = unix.MmapPtr(fds[r.FD], int64(r.Offset), unsafe.Add(unsafe.Pointer(unsafe.SliceData(mapping)), uintptr(r.Start)), uintptr(r.Length), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED|unix.MAP_FIXED)
		if err != nil {
			unix.Munmap(mapping)
			return memory{}, err
		}
		covered += r.Length
	}
	if covered != length {
		unix.Munmap(mapping)
		return memory{}, errors.New("incomplete temporary memory mapping")
	}
	c.mappings = append(c.mappings, mapping)
	memory := memory{Data: mapping[:res.Size:res.Size], Addr: res.Address}
	if res.Errno != 0 {
		return memory, unix.Errno(res.Errno)
	}
	return memory, nil
}

func (c *syscallEvent) readString(address uint64) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, fds, err := c.request(intercept.Message{Op: "string", Address: address})
	for _, fd := range fds {
		unix.Close(fd)
	}
	if err == nil && res.Errno != 0 {
		err = unix.Errno(res.Errno)
	}
	return res.Path, err
}

// resolvePath uses guest credentials and its actual cwd/FD table. A missing
// final component is reported as ENOENT. Link holds an unfollowed symlink.
func (c *syscallEvent) resolvePath(dirfd int32, path string, follow bool) (intercept.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, fds, err := c.request(intercept.Message{Op: "path", Path: path, DirFD: dirfd, Follow: follow})
	for _, fd := range fds {
		unix.Close(fd)
	}
	if err == nil && res.Errno != 0 {
		err = unix.Errno(res.Errno)
	}
	return res, err
}

// setWorkingDirectory updates a bootstrap cwd before guest execution resumes.
func (c *syscallEvent) setWorkingDirectory(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, fds, err := c.request(intercept.Message{Op: "cwd", Path: path, Follow: true})
	for _, fd := range fds {
		unix.Close(fd)
	}
	if err == nil && res.Errno != 0 {
		err = unix.Errno(res.Errno)
	}
	return err
}

// copyOut writes a syscall output into an existing guest buffer.
func (c *syscallEvent) copyOut(address uint64, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	res, fds, err := c.request(intercept.Message{Op: "copyout", Address: address, Data: data})
	for _, fd := range fds {
		unix.Close(fd)
	}
	if err == nil && res.Errno != 0 {
		err = unix.Errno(res.Errno)
	}
	return err
}

func (c *syscallEvent) finish(callbackErr error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active = false
	for _, mapping := range c.mappings {
		if err := unix.Munmap(mapping); err != nil {
			callbackErr = errors.Join(callbackErr, err)
		}
	}
	c.mappings = nil
	res := intercept.Message{Op: "resume", Args: c.Args, Result: c.Result}
	if callbackErr != nil {
		res.Error = callbackErr.Error()
	}
	data, err := json.Marshal(res)
	if err != nil {
		return err
	}
	n, err := c.conn.Write(data)
	if err == nil && n != len(data) {
		err = fmt.Errorf("resume: %w", io.ErrShortWrite)
	}
	return err
}
