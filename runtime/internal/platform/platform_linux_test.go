//go:build linux && (amd64 || arm64)

package platform

import (
	"reflect"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

type switchFixture struct {
	platform.Context
	info     *linux.SignalInfo
	access   hostarch.AccessType
	err      error
	ctx      context.Context
	mm       platform.MemoryManager
	ac       *arch.Context64
	cpu      int32
	released bool
}

func (f *switchFixture) Switch(ctx context.Context, mm platform.MemoryManager, ac *arch.Context64, cpu int32) (*linux.SignalInfo, hostarch.AccessType, error) {
	f.ctx, f.mm, f.ac, f.cpu = ctx, mm, ac, cpu
	return f.info, f.access, f.err
}

func (f *switchFixture) Release() { f.released = true }

func TestSwitchPreservesExecution(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
	}{
		{name: "syscall"},
		{name: "signal", err: platform.ErrContextSignal},
		{name: "interrupt", err: platform.ErrContextInterrupt},
		{name: "preemption", err: platform.ErrContextCPUPreempted},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := &switchFixture{info: &linux.SignalInfo{Signo: int32(linux.SIGSEGV)}, access: hostarch.Read, err: test.err}
			observer := observedContext{Context: base}
			ctx := context.Background()
			var ac arch.Context64
			before := ac
			info, access, err := observer.Switch(ctx, nil, &ac, 3)
			if info != base.info || access != base.access || err != test.err {
				t.Fatalf("Switch changed the underlying result: %v, %v, %v", info, access, err)
			}
			if base.ctx != ctx || base.mm != nil || base.ac != &ac || base.cpu != 3 || !reflect.DeepEqual(ac, before) {
				t.Fatal("Switch changed the execution inputs or register state")
			}
			want := uint64(0)
			if test.err == nil {
				want = 1
			}
			if observer.syscalls != want {
				t.Fatalf("observed %d syscalls, want %d", observer.syscalls, want)
			}
			observer.Release()
			if !base.released {
				t.Fatal("Release did not reach the underlying context")
			}
		})
	}
}

func TestPlatformRegistration(t *testing.T) {
	registered, err := platform.Lookup("successbox")
	if err != nil {
		t.Fatal(err)
	}
	wrapper, ok := registered.(*constructor)
	if !ok {
		t.Fatalf("registered constructor is %T", registered)
	}
	base, err := platform.Lookup("systrap")
	if err != nil {
		t.Fatal(err)
	}
	if wrapper.Constructor != base || wrapper.Requirements() != base.Requirements() {
		t.Fatal("registration did not retain the existing systrap constructor")
	}
}
