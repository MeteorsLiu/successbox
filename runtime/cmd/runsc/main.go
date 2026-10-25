//go:build linux && (amd64 || arm64)

package main

import (
	_ "github.com/MeteorsLiu/successbox/runtime/internal/platform"
	"gvisor.dev/gvisor/runsc/cli/maincli"
)

func main() {
	maincli.Main()
}
