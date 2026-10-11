package cmd

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

var sleepCmd = &cobra.Command{
	Use:   "sleep <duration>",
	Short: "Idle for a duration (keeps a pod alive for kubectl exec)",
	Long: `Do nothing for the given duration, then exit 0. The amp-cli image is
distroless and has no shell or sleep binary, so this is how a pod is kept
running for interactive use with kubectl exec. Exits early on SIGTERM/SIGINT.

Example:
  amp-cli sleep 1h
  kubectl exec -it job/amp-cli-interactive -- amp-cli chat --agent-url http://chatbot:8080`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		d, err := time.ParseDuration(args[0])
		if err != nil || d < 0 {
			return fmt.Errorf("invalid duration %q: use a Go duration such as 30m or 1h", args[0])
		}

		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		sleepFor(ctx, d)
		return nil
	},
}

// sleepFor blocks for d or until ctx is done, whichever comes first.
func sleepFor(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
