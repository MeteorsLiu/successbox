//go:build linux && (amd64 || arm64)

// Package observe registers a systrap wrapper for the syscall interception probe.
// Import it into both the runsc launcher and the Sentry executable: registration
// only affects the importing process, not the child executable it launches.
package observe

import (
	"errors"
	"sync/atomic"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	_ "gvisor.dev/gvisor/pkg/sentry/platform/systrap"
)

// Set only during package initialization. The default observer does not allocate
// memory; the build-tagged integration probe installs a syscall inspector.
var inspectSyscall func(*syscallMemory, *arch.Context64) error

func init() {
	base, err := platform.Lookup("systrap")
	if err != nil {
		panic(err)
	}
	platform.Register("successbox", &constructor{Constructor: base})
}

type constructor struct {
	platform.Constructor
}

func (c *constructor) New(opts platform.Options) (platform.Platform, error) {
	if inspectSyscall != nil {
		installSyscallMemory()
	}
	base, err := c.Constructor.New(opts)
	if err != nil {
		return nil, err
	}
	log.Infof("successbox-platform: observer enabled in Sentry")
	return &observedPlatform{Platform: base}, nil
}

type observedPlatform struct {
	platform.Platform
	nextContext atomic.Uint64
}

func (p *observedPlatform) NewContext(ctx context.Context) platform.Context {
	return &observedContext{
		Context: p.Platform.NewContext(ctx),
		id:      p.nextContext.Add(1),
		task:    kernel.TaskFromContext(ctx),
	}
}

type observedContext struct {
	platform.Context
	id       uint64
	syscalls uint64
	unames   uint64
	task     *kernel.Task
}

func (c *observedContext) Switch(ctx context.Context, mm platform.MemoryManager, ac *arch.Context64, cpu int32) (*linux.SignalInfo, hostarch.AccessType, error) {
	info, access, err := c.Context.Switch(ctx, mm, ac, cpu)
	// nil means a syscall trap, before Sentry dispatches the syscall. Signals
	// and preemption also return through Switch and must bypass the observer.
	if err == nil {
		c.syscalls++
		if ac.SyscallNo() == uintptr(unix.SYS_UNAME) {
			c.unames++
			log.Debugf("successbox-platform: context=%d observed uname", c.id)
		}
		if inspectSyscall != nil {
			ac.SyscallSaveOrig()
			m := &syscallMemory{active: true, task: c.task, mm: c.task.MemoryManager(), ctx: c.task.Kernel().SupervisorContext()}
			inspectionErr := inspectSyscall(m, ac)
			m.active = false
			if inspectionErr != nil {
				return nil, hostarch.NoAccess, errors.Join(inspectionErr, m.release())
			}
			if len(m.regions) != 0 {
				memoryCalls.Lock()
				memoryCalls.pending[c.task] = m
				memoryCalls.live[m] = struct{}{}
				memoryCalls.Unlock()
			}
		}
	}
	return info, access, err
}

func (c *observedContext) Release() {
	// A Context has one execution owner. Release runs after its last Switch,
	// so the counts need no shared map or per-syscall synchronization.
	if c.task != nil {
		if err := releaseTaskMemory(c.task); err != nil {
			log.Warningf("successbox-platform: releasing retained syscall memory: %v", err)
		}
	}
	c.Context.Release()
	log.Infof("successbox-platform: context=%d syscalls=%d uname=%d", c.id, c.syscalls, c.unames)
}
