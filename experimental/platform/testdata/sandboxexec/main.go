//go:build linux && (amd64 || arm64)

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

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
		return fmt.Errorf("usage: sandboxexec-probe ROOTFS RESULT_DIRECTORY")
	}
	rootfs, directory := os.Args[1], os.Args[2]
	debug := filepath.Join(directory, "debug")
	if err := os.MkdirAll(debug, 0700); err != nil {
		return err
	}
	ctx := context.Background()
	box, err := sandbox.New(ctx,
		sandbox.WithRootfs(rootfs, false),
		sandbox.WithRuntimeDir(filepath.Join(directory, "runtime")),
		sandbox.WithStateDir(filepath.Join(directory, "state")),
		sandbox.WithDebug(debug+"/"),
		sandbox.WithoutHostBinaryMounts(),
	)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := box.Close(ctx); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	result, err := box.Exec(ctx, []string{"/bin/uname", "-s"})
	if err != nil || result.ExitCode != 0 || result.Stdout != "Linux\n" {
		return fmt.Errorf("uname: result=%+v, error=%v", result, err)
	}
	input := bytes.Repeat([]byte{0, 255, 1, 128, 13, 10}, 8192)
	var output bytes.Buffer
	result, err = box.Exec(ctx, []string{"/bin/cat"}, sandbox.WithExecStdio(bytes.NewReader(input), &output, nil))
	if err != nil || result.ExitCode != 0 || !bytes.Equal(input, output.Bytes()) {
		return fmt.Errorf("binary IO: result=%+v, bytes=%d, error=%v", result, output.Len(), err)
	}
	result, err = box.Exec(ctx, []string{"/bin/sh", "-c", "printf output; printf error >&2; exit 7"})
	if err != nil || result.ExitCode != 7 || result.Stdout != "output" || result.Stderr != "error" {
		return fmt.Errorf("exit and streams: result=%+v, error=%v", result, err)
	}
	var calls sync.WaitGroup
	errors := make(chan error, 4)
	for range 4 {
		calls.Add(1)
		go func() {
			defer calls.Done()
			result, err := box.Exec(ctx, []string{"/bin/uname", "-s"})
			if err != nil || result.ExitCode != 0 || result.Stdout != "Linux\n" {
				errors <- fmt.Errorf("concurrent uname: result=%+v, error=%v", result, err)
			}
		}()
	}
	calls.Wait()
	close(errors)
	for err := range errors {
		return err
	}
	fmt.Println("sandboxexec: uname, binary IO, stderr, exit status, and concurrent Exec passed")
	return nil
}
