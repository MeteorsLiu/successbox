package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The authorized account exists only in the host proxy's temporary store.
// The agent gets a synthetic gateway key, never the user's auth.json.
func gatewayCodexLLMTestBackend(t *testing.T, auth []byte) string {
	t.Helper()
	if os.Getenv("SUCCESSBOX_TEST_CLIPROXY") == "" || os.Getenv("SUCCESSBOX_TEST_CLIPROXY_PLUGIN") == "" {
		t.Fatal("live agent E2E requires SUCCESSBOX_TEST_CLIPROXY and SUCCESSBOX_TEST_CLIPROXY_PLUGIN")
	}
	dir := t.TempDir()
	var source struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
			AccountID   string `json:"account_id"`
			IDToken     string `json:"id_token"`
		} `json:"tokens"`
	}
	if json.Unmarshal(auth, &source) != nil || source.Tokens.AccessToken == "" || source.Tokens.AccountID == "" {
		t.Fatal("live proxy test requires an authorized Codex subscription auth file")
	}
	authDir := filepath.Join(dir, "oauth")
	if err := os.Mkdir(authDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Do not import refresh_token: this bounded test must not rotate the
	// credentials used by the user's other sessions.
	credentials, _ := json.Marshal(map[string]string{"type": "codex", "access_token": source.Tokens.AccessToken, "account_id": source.Tokens.AccountID, "id_token": source.Tokens.IDToken})
	if err := os.WriteFile(filepath.Join(authDir, "authorized.json"), credentials, 0600); err != nil {
		t.Fatal(err)
	}
	extension := ".so"
	if runtime.GOOS == "darwin" {
		extension = ".dylib"
	}
	pluginDir := filepath.Join(dir, "plugins")
	pluginPath := filepath.Join(pluginDir, runtime.GOOS, runtime.GOARCH, "successbox"+extension)
	if err := os.MkdirAll(filepath.Dir(pluginPath), 0700); err != nil {
		t.Fatal(err)
	}
	plugin, err := os.ReadFile(os.Getenv("SUCCESSBOX_TEST_CLIPROXY_PLUGIN"))
	if err != nil {
		t.Fatal("read test CLIProxyAPI plugin:", err)
	}
	if err := os.WriteFile(pluginPath, plugin, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	config := map[string]any{
		"config-version": 8,
		"server":         map[string]any{"host": "127.0.0.1", "port": port},
		"management":     map[string]any{"secret-key": "", "disable-control-panel": true},
		"access":         map[string]any{"api-keys": []string{"successbox-live-client"}},
		"oauth":          map[string]any{"auth-dir": authDir},
		"observability":  map[string]any{"logs": map[string]any{"request-log": false, "debug": false}},
		"routing":        map[string]any{"retry": map[string]any{"request-retry": 0}},
		"plugins":        map[string]any{"enabled": true, "dir": pluginDir, "configs": map[string]any{"successbox": map[string]any{"enabled": true, "bindings": map[string]any{"successbox-live-client": map[string]string{"codex": "authorized.json"}}}}},
	}
	raw, _ := json.Marshal(config)
	configPath := filepath.Join(dir, "llm.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	log, err := os.OpenFile(filepath.Join(dir, "cliproxy.log"), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Getenv("SUCCESSBOX_TEST_CLIPROXY"), "-config", configPath, "-local-model")
	command.Dir = dir
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout, command.Stderr = log, log
	if err := command.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- command.Wait() }()
	t.Cleanup(func() {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			t.Error("CLIProxyAPI did not stop after SIGTERM")
		}
		_ = log.Close()
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer successbox-live-client")
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("CLIProxyAPI readiness deadline exceeded; private log withheld")
		}
		time.Sleep(50 * time.Millisecond)
	}
	target, _ := url.Parse(base)
	proxy := httputil.NewSingleHostReverseProxy(target)
	var calls atomic.Int32
	observed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/responses" || r.URL.Path == "/v1/messages" {
			calls.Add(1)
			if r.Header.Get("Authorization") != "Bearer successbox-live-client" && r.Header.Get("x-api-key") != "successbox-live-client" {
				t.Error("agent did not use its gateway credential")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}

		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		observed.Close()
		if calls.Load() < 2 {
			t.Errorf("expected both model turns through CLIProxyAPI, got %d requests", calls.Load())
		} else {
			t.Logf("observed %d model requests through the real LLM gateway", calls.Load())
		}
	})
	return observed.URL + "/v1"
}
