package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// gateways composes the native gateway programs. Their own configuration and
// credential stores remain authoritative; sbx does not copy upstream secrets.
func gateways(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("gateways", flag.ContinueOnError)
	flags.SetOutput(output)
	llmBinary := flags.String("cliproxy", "cli-proxy-api", "CLIProxyAPI executable")
	llmConfig := flags.String("llm-config", "", "CLIProxyAPI JSON configuration file")
	mcpBinary := flags.String("bifrost", "bifrost-http", "Bifrost executable")
	mcpDir := flags.String("mcp-dir", "", "Bifrost directory containing config.json and its database")
	mcpPort := flags.Uint("mcp-port", 8081, "Bifrost loopback HTTP port")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *llmConfig == "" || *mcpDir == "" || *mcpPort == 0 || *mcpPort > 65535 {
		return errors.New("gateways requires --llm-config, --mcp-dir and a valid --mcp-port")
	}
	llmPath, err := filepath.Abs(*llmConfig)
	if err != nil {
		return err
	}
	mcpPath, err := filepath.Abs(*mcpDir)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(llmPath)
	if err != nil {
		return fmt.Errorf("read LLM config: %w", err)
	}
	var llm struct {
		Server struct {
			Host string
			Port int
		}
	}
	if err := json.Unmarshal(data, &llm); err != nil {
		return errors.New("LLM config must be a JSON object accepted by CLIProxyAPI")
	}
	address := net.ParseIP(llm.Server.Host)
	if address == nil || !address.IsLoopback() || llm.Server.Port < 1 || llm.Server.Port > 65535 {
		return errors.New("LLM config server.host must be a loopback IP with a valid server.port")
	}
	if llm.Server.Port == int(*mcpPort) {
		return errors.New("LLM and MCP gateways require different ports")
	}
	data, err = os.ReadFile(filepath.Join(mcpPath, "config.json"))
	if err != nil {
		return fmt.Errorf("read MCP config: %w", err)
	}
	var config map[string]json.RawMessage
	if err := json.Unmarshal(data, &config); err != nil || config == nil {
		return errors.New("MCP config must be a JSON object")
	}
	llmExecutable, err := exec.LookPath(*llmBinary)
	if err != nil {
		return err
	}
	llmExecutable, err = filepath.Abs(llmExecutable)
	if err != nil {
		return err
	}
	mcpExecutable, err := exec.LookPath(*mcpBinary)
	if err != nil {
		return err
	}
	mcpExecutable, err = filepath.Abs(mcpExecutable)
	if err != nil {
		return err
	}
	commands := []*exec.Cmd{
		exec.Command(llmExecutable, "-config", llmPath, "-local-model"),
		exec.Command(mcpExecutable, "-host", "127.0.0.1", "-port", fmt.Sprint(*mcpPort), "-app-dir", mcpPath, "-log-level", "warn"),
	}
	commands[0].Dir = filepath.Dir(llmPath)
	commands[1].Dir = mcpPath
	return superviseGateways(ctx, commands, output)
}

// A gateway pair belongs to one foreground command. If either exits, stop both
// process groups, including local MCP children Bifrost started over stdio.
func superviseGateways(ctx context.Context, commands []*exec.Cmd, output io.Writer) (err error) {
	type exit struct {
		index int
		err   error
	}
	exits := make(chan exit, len(commands))
	started := 0
	remaining := 0
	defer func() {
		for i := 0; i < started; i++ {
			signalErr := syscall.Kill(-commands[i].Process.Pid, syscall.SIGTERM)
			if signalErr != nil && !errors.Is(signalErr, syscall.ESRCH) {
				err = errors.Join(err, signalErr)
			}
		}
		timer := time.NewTimer(10 * time.Second)
		defer timer.Stop()
		for remaining > 0 {
			select {
			case <-exits:
				remaining--
			case <-timer.C:
				for i := 0; i < started; i++ {
					_ = syscall.Kill(-commands[i].Process.Pid, syscall.SIGKILL)
				}
			}
		}
	}()
	for i, command := range commands {
		if err := ctx.Err(); err != nil {
			return err
		}
		command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		command.Stdout, command.Stderr = output, output
		if err := command.Start(); err != nil {
			return fmt.Errorf("start %s: %w", filepath.Base(command.Path), err)
		}
		started++
		remaining++
		go func() { exits <- exit{index: i, err: command.Wait()} }()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case result := <-exits:
		remaining--
		if result.err == nil {
			return fmt.Errorf("%s exited; gateway pair stopped", filepath.Base(commands[result.index].Path))
		}
		return fmt.Errorf("%s exited: %w", filepath.Base(commands[result.index].Path), result.err)
	}
}
