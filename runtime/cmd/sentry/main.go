//go:build linux && (amd64 || arm64)

package main

import (
	_ "github.com/MeteorsLiu/successbox/runtime/internal/platform"
	"gvisor.dev/gvisor/runsc/cli"
	"gvisor.dev/gvisor/runsc/cmd/sentry/sentrycmd"
	"gvisor.dev/gvisor/runsc/cmd/util"
	"gvisor.dev/gvisor/runsc/gvisorbinaries"
)

func main() {
	cli.Run(&gvisorbinaries.GvisorSentry, map[util.SubCommand]string{
		new(sentrycmd.Boot):   "internal use only",
		new(sentrycmd.Umount): "internal use only",
	}, nil)
}
