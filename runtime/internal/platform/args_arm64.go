//go:build linux && arm64

package platform

import "gvisor.dev/gvisor/pkg/sentry/arch"

func setSyscallArgs(ac *arch.Context64, args arch.SyscallArguments) {
	for i, arg := range args {
		ac.Regs.Regs[i] = arg.Uint64()
	}
	ac.SyscallSaveOrig()
}
