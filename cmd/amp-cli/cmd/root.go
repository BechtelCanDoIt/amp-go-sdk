package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/wso2/amp-go/amp"
)

var (
	// Global flags
	agentURL  string
	userToken string
	debug     bool

	// Shared SDK client — initialized in PersistentPreRun for subcommands
	// that need it (trace, token, health). Chat and send use it optionally.
	client *amp.AMPClient
)

var rootCmd = &cobra.Command{
	Use:   "amp-cli",
	Short: "AMP Agent CLI — test harness for WSO2 Agent Manager agents",
	Long: `amp-cli is an interactive test tool for WSO2 Agent Manager agents.
It runs inside the Kubernetes cluster to reach obs-gateway and AMP API
via in-cluster DNS.

Subcommands:
  chat    Interactive multi-turn REPL
  send    Single-shot message (CI-friendly)
  trace   Print recent traces from amp-trace-observer
  token   Fetch and print a fresh agent JWT
  health  Verify connectivity to AMP API, obs-gateway, OTLP collector`,
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	rootCmd.PersistentFlags().StringVar(&agentURL, "agent-url", envDefault("AGENT_URL", "http://localhost:8080"), "Agent HTTP endpoint")
	rootCmd.PersistentFlags().StringVar(&userToken, "user-token", os.Getenv("X_USER_TOKEN"), "Employee JWT for least-privilege testing")
	rootCmd.PersistentFlags().BoolVar(&debug, "debug", false, "Enable verbose output")

	rootCmd.AddCommand(chatCmd)
	rootCmd.AddCommand(sendCmd)
	rootCmd.AddCommand(traceCmd)
	rootCmd.AddCommand(tokenCmd)
	rootCmd.AddCommand(healthCmd)
}

// initSDKClient initializes the shared AMP SDK client. Called by subcommands
// that need direct AMP API access (trace, token, health).
func initSDKClient() (*amp.AMPClient, error) {
	if client != nil {
		return client, nil
	}

	cfg := amp.FromEnv()
	if debug {
		cfg.Debug = true
	}

	c, err := amp.Init(cfg)
	if err != nil {
		return nil, fmt.Errorf("SDK init failed: %w", err)
	}

	client = c

	// Wire up shutdown on signal.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		client.Shutdown(ctx)
	}()

	return client, nil
}

func envDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
