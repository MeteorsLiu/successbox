package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
)

func init() {
	worktreeNamespace = func() error {
		if os.Getenv("SUCCESSBOX_MOUNT_NAMESPACE") == "1" {
			return unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, "")
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		cmd := exec.Command(executable, os.Args[1:]...)
		cmd.Env = append(os.Environ(), "SUCCESSBOX_MOUNT_NAMESPACE=1")
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: unix.CLONE_NEWNS}
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			return err
		}
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(signals)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		for {
			select {
			case sig := <-signals:
				_ = cmd.Process.Signal(sig)
			case err := <-done:
				if err != nil {
					return err
				}
				os.Exit(0)
			}
		}
	}
	worktreeMounts = func(spec *specs.Spec, dir string) (cleanup func() error, err error) {
		var mounted []string
		cleanup = func() error {
			var result error
			for i := len(mounted) - 1; i >= 0; i-- {
				result = errors.Join(result, unix.Unmount(mounted[i], unix.MNT_DETACH))
			}
			return result
		}
		defer func() {
			if err != nil {
				err = errors.Join(err, cleanup())
				cleanup = nil
			}
		}()
		// A separate root entry preserves child mounts during runsc's gofer setup.
		root := filepath.Join(dir, "rootfs")
		if err = os.Mkdir(root, 0700); err != nil {
			return
		}
		if err = unix.Mount(spec.Root.Path, root, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return
		}
		mounted = append(mounted, root)
		spec.Root.Path = root
		// runsc overlays the root. Each writable user bind needs its own disposable
		// upper; the final results bind is the only writable host directory.
		for i := range spec.Mounts {
			mount := &spec.Mounts[i]
			if mount.Type != "bind" || i == len(spec.Mounts)-1 {
				continue
			}
			// State aliases are completely covered by the later tmpfs mask.
			// They must not use an upper directory inside their own lower tree.
			for _, later := range spec.Mounts[i+1:] {
				if later.Destination == mount.Destination && later.Type == "tmpfs" {
					mount.Options = append(mount.Options, "ro")
					break
				}
			}
			readonly := false
			for _, option := range mount.Options {
				if option == "ro" {
					readonly = true
				}
			}
			if readonly {
				continue
			}
			source, sourceErr := filepath.EvalSymlinks(mount.Source)
			if sourceErr != nil {
				err = sourceErr
				return
			}
			info, statErr := os.Stat(source)
			if statErr != nil {
				err = statErr
				return
			}
			lower := source
			if !info.IsDir() {
				lower = filepath.Dir(source)
			}
			base := filepath.Join(dir, fmt.Sprintf("bind-%d", i))
			// The service state directory can itself be on overlayfs (for
			// example in an Apple-managed Linux environment). Use a tmpfs
			// upper for disposable bind writes rather than nesting uppers.
			if err = os.MkdirAll(base, 0700); err != nil {
				return
			}
			if err = unix.Mount("tmpfs", base, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=700"); err != nil {
				return
			}
			mounted = append(mounted, base)
			for _, name := range []string{"upper", "work", "merged"} {
				if err = os.MkdirAll(filepath.Join(base, name), 0700); err != nil {
					return
				}
			}
			var lowerStat unix.Stat_t
			if err = unix.Stat(lower, &lowerStat); err != nil {
				return
			}
			upper := filepath.Join(base, "upper")
			if err = os.Chown(upper, int(lowerStat.Uid), int(lowerStat.Gid)); err != nil {
				return
			}
			if err = unix.Chmod(upper, lowerStat.Mode&07777); err != nil {
				return
			}
			// Overlayfs mount data uses backslash escapes for path delimiters.
			escape := strings.NewReplacer("\\", "\\\\", ",", "\\,", ":", "\\:").Replace
			data := "lowerdir=" + escape(lower) + ",upperdir=" + escape(filepath.Join(base, "upper")) + ",workdir=" + escape(filepath.Join(base, "work"))
			target := filepath.Join(base, "merged")
			if err = unix.Mount("overlay", target, "overlay", unix.MS_NOSUID|unix.MS_NODEV, data); err != nil {
				err = fmt.Errorf("private bind %s: %w", mount.Destination, err)
				return
			}
			mounted = append(mounted, target)
			mount.Source = target
			if !info.IsDir() {
				mount.Source = filepath.Join(target, filepath.Base(source))
			}
		}
		return
	}
}
