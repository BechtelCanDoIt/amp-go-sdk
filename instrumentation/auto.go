// Package instrumentation provides zero-code auto-initialization for the
// amp-go SDK, equivalent to Python's sitecustomize.py mechanism.
//
// Usage — blank import in your main.go or any init-order-relevant file:
//
//	import _ "github.com/wso2/amp-go/instrumentation"
//
// The package-level init() function runs before main() and:
//  1. Reads all AMP_* environment variables via FromEnv().
//  2. Validates required variables — panics with a clear message if absent.
//  3. Calls amp.Init() to acquire the agent JWT, configure OTLP, and start
//     the background refresh goroutine.
//  4. Registers a SIGTERM/SIGINT handler that calls Shutdown() before exit.
//
// For cases where explicit control is preferred, use amp.Init(cfg) directly
// instead of this package.
package instrumentation

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wso2/amp-go/amp"
)

// Client is the globally-accessible AMPClient created by init().
// Agent code can use this after the blank import to access span helpers
// and the conversation manager without calling Init() themselves.
var Client *amp.AMPClient

func init() {
	cfg := amp.FromEnv()

	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("amp-go auto-init failed: %v\n\n"+
			"The amp-go instrumentation package requires these environment variables:\n"+
			"  AMP_OTEL_ENDPOINT    — OTLP HTTP endpoint (obs-gateway)\n"+
			"  AMP_AGENT_API_KEY    — Agent API key from AMP Console\n"+
			"  AMP_BASE_URL         — AMP API base URL\n"+
			"  AMP_ORG_NAME         — Organization name\n"+
			"  AMP_PROJECT_NAME     — Project name\n"+
			"  AMP_AGENT_NAME       — Agent name\n"+
			"\nSet the missing variables and restart.", err))
	}

	client, err := amp.Init(cfg)
	if err != nil {
		panic(fmt.Sprintf("amp-go auto-init failed: %v", err))
	}

	Client = client

	// Register signal handler for clean shutdown.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		sig := <-sigCh

		fmt.Fprintf(os.Stderr, "\namp-go: received %s, shutting down...\n", sig)

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := client.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "amp-go: shutdown error: %v\n", err)
			os.Exit(1)
		}

		os.Exit(0)
	}()
}
