// Package e2e tests the instrumentation package's zero-code auto-init.
//
// That package initializes in init(), which panics when the AMP environment is
// missing — so it cannot be imported by a test binary. These tests build a tiny
// agent that blank-imports it and run that as a subprocess instead.
package e2e

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// buildAgent compiles the sample auto-init agent once per test run.
func buildAgent(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "autoinit")
	out, err := exec.Command("go", "build", "-o", bin, "./testdata/autoinit").CombinedOutput()
	if err != nil {
		t.Fatalf("build sample agent: %v\n%s", err, out)
	}
	return bin
}

// runAgent runs the agent with only the given AMP_* variables set.
func runAgent(bin string, env ...string) (stdout, stderr string, err error) {
	cmd := exec.Command(bin)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "AMP_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, env...)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &outBuf, &errBuf
	err = cmd.Run()
	return outBuf.String(), errBuf.String(), err
}

func TestAutoInit(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs a subprocess")
	}
	bin := buildAgent(t)

	t.Run("exports spans with only the two required variables", func(t *testing.T) {
		var (
			mu    sync.Mutex
			paths []string
			keys  []string
		)
		otlp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			paths = append(paths, r.Method+" "+r.URL.Path)
			keys = append(keys, r.Header.Get("x-amp-api-key"))
			mu.Unlock()
		}))
		defer otlp.Close()

		stdout, stderr, err := runAgent(bin,
			"AMP_OTEL_ENDPOINT="+otlp.URL,
			"AMP_AGENT_API_KEY=auto-key",
		)
		if err != nil {
			t.Fatalf("agent failed: %v\nstderr: %s", err, stderr)
		}
		if !strings.Contains(stdout, "autoinit ok") {
			t.Errorf("stdout = %q, want the agent to reach main()", stdout)
		}

		mu.Lock()
		defer mu.Unlock()
		if len(paths) != 1 || paths[0] != "POST /v1/traces" {
			t.Fatalf("OTLP requests = %v, want one POST /v1/traces", paths)
		}
		if keys[0] != "auto-key" {
			t.Errorf("x-amp-api-key = %q, want auto-key", keys[0])
		}
	})

	t.Run("fails at startup when the environment is missing", func(t *testing.T) {
		stdout, stderr, err := runAgent(bin)
		if err == nil {
			t.Fatalf("agent should not start without AMP_* variables; stdout: %s", stdout)
		}
		if strings.Contains(stdout, "autoinit ok") {
			t.Error("main() ran despite the failed auto-init")
		}
		for _, want := range []string{"amp-go auto-init failed", "AMP_OTEL_ENDPOINT", "AMP_AGENT_API_KEY"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr missing %q:\n%s", want, stderr)
			}
		}
	})

	t.Run("fails fast when the configured control plane rejects the token request", func(t *testing.T) {
		otlp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		defer otlp.Close()
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer api.Close()

		_, stderr, err := runAgent(bin,
			"AMP_OTEL_ENDPOINT="+otlp.URL,
			"AMP_AGENT_API_KEY=auto-key",
			"AMP_BASE_URL="+api.URL,
			"AMP_ORG_NAME=org",
			"AMP_PROJECT_NAME=proj",
			"AMP_AGENT_NAME=agent",
		)
		if err == nil {
			t.Fatal("agent should not start when token acquisition fails")
		}
		if !strings.Contains(stderr, "initial token acquisition failed") {
			t.Errorf("stderr should explain the token failure:\n%s", stderr)
		}
	})
}
