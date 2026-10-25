//go:build linux

package main

import (
	"fmt"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
)

type sandboxNetwork struct {
	helper, slirp *exec.Cmd
	path          string
	once          sync.Once
}

func createSandboxNetwork(dir, slirp string) (*sandboxNetwork, error) {
	helper := exec.Command("sleep", "infinity")
	helper.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNET}
	if err := helper.Start(); err != nil {
		return nil, fmt.Errorf("create network namespace: %w", err)
	}
	n := &sandboxNetwork{helper: helper, path: filepath.Join("/proc", fmt.Sprint(helper.Process.Pid), "ns/net")}
	readyR, readyW, err := os.Pipe()
	if err != nil {
		n.cleanup()
		return nil, err
	}
	logFile, err := os.OpenFile(filepath.Join(dir, "network.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		readyR.Close()
		readyW.Close()
		n.cleanup()
		return nil, err
	}
	n.slirp = exec.Command(slirp, "--configure", "--disable-host-loopback", "--ready-fd=3", fmt.Sprint(helper.Process.Pid), "tap0")
	n.slirp.ExtraFiles = []*os.File{readyW}
	n.slirp.Stderr = logFile
	if err := n.slirp.Start(); err != nil {
		logFile.Close()
		readyR.Close()
		readyW.Close()
		n.cleanup()
		return nil, err
	}
	readyW.Close()
	var marker [1]byte
	_, readErr := io.ReadFull(readyR, marker[:])
	readyR.Close()
	logFile.Close()
	if readErr != nil || marker[0] != '1' {
		n.cleanup()
		return nil, fmt.Errorf("slirp4netns did not become ready: %w", readErr)
	}
	return n, nil
}

func (n *sandboxNetwork) cleanup() {
	if n == nil {
		return
	}
	n.once.Do(func() {
		if n.slirp != nil && n.slirp.Process != nil {
			_ = n.slirp.Process.Signal(syscall.SIGTERM)
			_ = n.slirp.Wait()
		}
		if n.helper != nil && n.helper.Process != nil {
			_ = n.helper.Process.Signal(syscall.SIGTERM)
			_ = n.helper.Wait()
		}
	})
}
