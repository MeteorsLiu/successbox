package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestGatewaysRequireBindingPlugin(t *testing.T) {
	for _, mode := range []string{"global disabled", "plugin disabled", "not configured", "missing", "directory"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			extension := ".so"
			if runtime.GOOS == "darwin" {
				extension = ".dylib"
			}
			pluginPath := filepath.Join(dir, "plugins", runtime.GOOS, runtime.GOARCH, "successbox"+extension)
			if mode == "directory" {
				if err := os.MkdirAll(pluginPath, 0700); err != nil {
					t.Fatal(err)
				}
			}
			config, err := json.Marshal(map[string]any{
				"server": map[string]any{"host": "127.0.0.1", "port": 8317},
				"plugins": map[string]any{
					"enabled": mode != "global disabled", "dir": "plugins",
					"configs": map[string]any{"successbox": map[string]any{"enabled": mode != "plugin disabled"}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "llm.json")
			if err := os.WriteFile(path, config, 0600); err != nil {
				t.Fatal(err)
			}
			err = gateways(context.Background(), []string{"--llm-config", path, "--mcp-dir", dir}, io.Discard)
			if mode == "not configured" {
				config, err = json.Marshal(map[string]any{"server": map[string]any{"host": "127.0.0.1", "port": 8317}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, config, 0600); err != nil {
					t.Fatal(err)
				}
				err = gateways(context.Background(), []string{"--llm-config", path, "--mcp-dir", dir}, io.Discard)
			}
			if err == nil || !strings.Contains(err.Error(), "successbox token binding plugin") {
				t.Fatalf("unexpected deployment validation: %v", err)
			}
		})
	}
}

func TestGatewaysRequireLoopback(t *testing.T) {
	for _, host := range []string{"", "0.0.0.0", "192.0.2.1"} {
		t.Run(host, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "llm.json")
			if err := os.WriteFile(path, []byte(`{"server":{"host":`+strconv.Quote(host)+`,"port":8080}}`), 0600); err != nil {
				t.Fatal(err)
			}
			err := gateways(context.Background(), []string{"--llm-config", path, "--mcp-dir", dir}, io.Discard)
			if err == nil {
				t.Fatal("accepted non-loopback gateway")
			}
		})
	}
}

func TestGatewayPairLifecycle(t *testing.T) {
	for _, mode := range []string{"cancel", "exit", "start failure"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			dir := t.TempDir()
			pidFile := filepath.Join(dir, "pid")
			first := exec.Command("/bin/sh", "-c", `echo $$ > "$1"; exec sleep 60`, "fixture", pidFile)
			second := exec.Command("/bin/sh", "-c", "exec sleep 60")
			if mode == "exit" {
				second = exec.Command("/bin/sh", "-c", "sleep 0.1; exit 7")
			}
			if mode == "start failure" {
				second = exec.Command(filepath.Join(dir, "absent"))
			}
			done := make(chan error, 1)
			go func() { done <- superviseGateways(ctx, []*exec.Cmd{first, second}, io.Discard) }()
			if mode == "cancel" {
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, err := os.Stat(pidFile); err == nil {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("child did not start")
					}
					time.Sleep(10 * time.Millisecond)
				}
				cancel()
			}
			select {
			case err := <-done:
				if mode == "cancel" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel result: %v", err)
				}
				if mode != "cancel" && err == nil {
					t.Fatal("gateway failure was hidden")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("supervisor did not stop")
			}
			if first.ProcessState == nil {
				t.Fatal("first gateway was not reaped")
			}
			if err := syscall.Kill(first.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("gateway still running: %v", err)
			}
			if mode != "start failure" && second.ProcessState == nil {
				t.Fatal("second gateway was not reaped")
			}
		})
	}
}
