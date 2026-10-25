package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

// gateways composes the native gateway programs. Their own configuration and
// credential stores remain authoritative; sbx does not copy upstream secrets.
func gateways(ctx context.Context, args []string, output io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	flags := flag.NewFlagSet("gateways", flag.ContinueOnError)
	flags.SetOutput(output)
	llmBinary := flags.String("cliproxy", "cli-proxy-api", "CLIProxyAPI executable")
	llmConfig := flags.String("llm-config", "", "CLIProxyAPI JSON configuration file")
	mcpBinary := flags.String("bifrost", "bifrost-http", "Bifrost executable")
	mcpDir := flags.String("mcp-dir", "", "Bifrost directory containing config.json and its database")
	mcpPort := flags.Uint("mcp-port", 8081, "Bifrost loopback HTTP port")
	controlListen := flags.String("control-listen", "", "host HTTP/2 address for GatewaySession, for example 127.0.0.1:9090")
	controlDB := flags.String("control-db", "", "SQLite database for GatewaySession state")
	frontendToken := flags.String("control-frontend-token", "", "bearer token for the frontend control client")
	gatewayToken := flags.String("control-gateway-token", "", "bearer token for the MCP gateway control client")
	agentToken := flags.String("control-agent-token", "", "bearer token for the sandbox agent control client")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *llmConfig == "" || *mcpDir == "" || *mcpPort == 0 || *mcpPort > 65535 {
		return errors.New("gateways requires --llm-config, --mcp-dir and a valid --mcp-port")
	}
	if *controlListen != "" && *controlDB == "" {
		return errors.New("--control-db is required when --control-listen is set")
	}
	if *controlListen == "" && (*controlDB != "" || *frontendToken != "" || *gatewayToken != "" || *agentToken != "") {
		return errors.New("--control-listen is required for GatewaySession control options")
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
		Plugins struct {
			Enabled bool
			Dir     string
			Configs map[string]struct {
				Enabled bool
			}
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
	binding, configured := llm.Plugins.Configs["successbox"]
	// CLIProxyAPI falls back to ordinary scheduling when the binding plugin is
	// disabled or absent. Never start a gateway that could route a valid token
	// through that fallback.
	if !configured || !llm.Plugins.Enabled || !binding.Enabled {
		return errors.New("successbox token binding plugin must be configured and enabled")
	}
	pluginDir := llm.Plugins.Dir
	if pluginDir == "" {
		pluginDir = "plugins"
	}
	if !filepath.IsAbs(pluginDir) {
		pluginDir = filepath.Join(filepath.Dir(llmPath), pluginDir)
	}
	extension := ".so"
	if runtime.GOOS == "darwin" {
		extension = ".dylib"
	}
	pluginPath := filepath.Join(pluginDir, runtime.GOOS, runtime.GOARCH, "successbox"+extension)
	info, err := os.Stat(pluginPath)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("successbox token binding plugin must be a regular file at %s", pluginPath)
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
	var mcp *gatewayMCP
	var mcpListener net.Listener
	upstreamPort := int(*mcpPort)
	if *controlListen != "" {
		var native struct {
			Governance struct {
				Auth struct {
					Enabled  bool   `json:"is_enabled"`
					Username string `json:"admin_username"`
					Password string `json:"admin_password"`
				} `json:"auth_config"`
				Keys []struct {
					ID     string `json:"id"`
					Value  string `json:"value"`
					Active bool   `json:"is_active"`
				} `json:"virtual_keys"`
			} `json:"governance"`
		}
		if err := json.Unmarshal(data, &native); err != nil {
			return errors.New("invalid MCP management configuration")
		}
		if !native.Governance.Auth.Enabled || native.Governance.Auth.Username == "" || native.Governance.Auth.Password == "" {
			return errors.New("MCP installation requires Bifrost admin authentication in its private configuration")
		}
		keys := make(map[string]string)
		for _, key := range native.Governance.Keys {
			if key.Active && key.ID != "" && key.Value != "" {
				keys[key.ID] = key.Value
			}
		}
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", *mcpPort))
		if err != nil {
			return err
		}
		mcpListener = listener
		defer mcpListener.Close()
		upstream, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return err
		}
		upstreamPort = upstream.Addr().(*net.TCPAddr).Port
		if err := upstream.Close(); err != nil {
			return err
		}
		mcp = &gatewayMCP{base: fmt.Sprintf("http://127.0.0.1:%d", upstreamPort), username: native.Governance.Auth.Username, password: native.Governance.Auth.Password, keys: keys, sessions: make(map[string]*gatewayMCPSession), client: &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}}
	}
	commands := []*exec.Cmd{
		exec.Command(llmExecutable, "-config", llmPath, "-local-model"),
		exec.Command(mcpExecutable, "-host", "127.0.0.1", "-port", fmt.Sprint(upstreamPort), "-app-dir", mcpPath, "-log-level", "warn"),
	}
	commands[0].Dir = filepath.Dir(llmPath)
	commands[1].Dir = mcpPath
	controlStop, controlErrors, err := startGatewayControl(ctx, *controlListen, *controlDB, map[string]string{
		"frontend": *frontendToken,
		"gateway":  *gatewayToken,
		"agent":    *agentToken,
	}, mcp)
	if err != nil {
		return err
	}
	defer controlStop()
	var httpErrors <-chan error
	if mcp != nil {
		httpServer := &http.Server{Handler: http.HandlerFunc(mcp.serveHTTP)}
		defer httpServer.Close()
		errors := make(chan error, 1)
		httpErrors = errors
		go func() { errors <- httpServer.Serve(mcpListener) }()
	}
	gatewayDone := make(chan error, 1)
	go func() { gatewayDone <- superviseGateways(ctx, commands, output) }()
	select {
	case err := <-gatewayDone:
		return err
	case err := <-controlErrors:
		cancel()
		controlStop()
		<-gatewayDone
		return err
	case err := <-httpErrors:
		cancel()
		controlStop()
		<-gatewayDone
		return err
	}

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
