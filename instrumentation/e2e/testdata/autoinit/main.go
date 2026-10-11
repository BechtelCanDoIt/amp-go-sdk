// Command autoinit is the smallest agent that uses zero-code auto-init. The
// e2e test runs it as a subprocess, since the instrumentation package does its
// work in init() before main().
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/wso2/amp-go/amp"
	"github.com/wso2/amp-go/instrumentation"
)

func main() {
	if instrumentation.Client == nil {
		fmt.Fprintln(os.Stderr, "instrumentation.Client is nil")
		os.Exit(2)
	}

	instrumentation.Client.LLMSpan(context.Background(),
		amp.LLMRequest{Model: "gpt-4o", Provider: "openai"},
		func(context.Context) (amp.LLMResult, error) { return amp.LLMResult{Content: "hi"}, nil },
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := instrumentation.Client.Shutdown(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "shutdown:", err)
		os.Exit(3)
	}
	fmt.Println("autoinit ok")
}
