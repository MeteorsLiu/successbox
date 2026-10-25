//go:build linux && amd64

package platform

import "gvisor.dev/gvisor/pkg/sentry/arch"

func setSyscallArgs(ac *arch.Context64, args arch.SyscallArguments) {
	ac.Regs.Rdi, ac.Regs.Rsi, ac.Regs.Rdx = args[0].Uint64(), args[1].Uint64(), args[2].Uint64()
	ac.Regs.R10, ac.Regs.R8, ac.Regs.R9 = args[3].Uint64(), args[4].Uint64(), args[5].Uint64()
	ac.SyscallSaveOrig()
}
