//go:build linux && arm64 && successbox_mmap_probe

package observe

import "gvisor.dev/gvisor/pkg/sentry/arch"

func setProbeArgs(ac *arch.Context64, args arch.SyscallArguments) {
	for i, arg := range args {
		ac.Regs.Regs[i] = arg.Uint64()
	}
	ac.SyscallSaveOrig()
}
