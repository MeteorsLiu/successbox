//go:build linux && (amd64 || arm64)

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gvisor.dev/gvisor/sandboxexec/sandbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() (err error) {
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: memory-probe RESULT_DIRECTORY GUEST_EXECUTABLE")
	}
	directory, executable := os.Args[1], os.Args[2]
	debug := filepath.Join(directory, "debug")
	if err := os.MkdirAll(debug, 0700); err != nil {
		return err
	}
	ctx := context.Background()
	box, err := sandbox.New(ctx, sandbox.WithRootfs("/", false),
		sandbox.WithRuntimeDir(filepath.Join(directory, "runtime")),
		sandbox.WithStateDir(filepath.Join(directory, "state")),
		sandbox.WithDebug(debug+"/"), sandbox.WithoutHostBinaryMounts())
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := box.Close(ctx); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	for _, test := range []struct {
		mode string
		code int
		text string
	}{
		{text: "cleanup passed"},
		{mode: "exec"},
		{mode: "clean", text: "memory clean"},
		{mode: "restart", text: "restart retention passed"},
		{mode: "clean", text: "memory clean"},
		{mode: "failure", code: 137},
		{mode: "clean", text: "memory clean"},
	} {
		argv := []string{executable}
		if test.mode != "" {
			argv = append(argv, test.mode)
		}
		result, err := box.Exec(ctx, argv)
		if err != nil {
			return err
		}
		if result.ExitCode != test.code || !strings.Contains(result.Stdout, test.text) {
			return fmt.Errorf("memory probe %q: %+v", test.mode, result)
		}
		fmt.Printf("memory probe %q: exit=%d %s", test.mode, result.ExitCode, result.Stdout)
		if result.Stdout == "" {
			fmt.Println()
		}
	}
	return nil
}
