package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wso2/amp-go/amp"
)

var healthCmd = &cobra.Command{
	Use:   "health",
	Short: "Verify connectivity to AMP API, obs-gateway, and OTLP collector",
	Long: `Check that all upstream services required by the SDK are reachable.
Useful for validating the K8s deployment and in-cluster DNS resolution.

Example:
  amp-cli health`,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := initSDKClient()
		if err != nil {
			return err
		}
		runHealthCheck(c)
		return nil
	},
}

func runHealthCheck(c *amp.AMPClient) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	results := c.Health(ctx)

	fmt.Println("\nAMP Health Check")
	fmt.Println(strings.Repeat("─", 60))

	allHealthy := true
	for _, r := range results {
		status := "✓"
		if !r.Healthy {
			status = "✗"
			allHealthy = false
		}

		fmt.Printf("  %s %-20s", status, r.Name)
		if r.URL != "" {
			fmt.Printf(" (%s)", r.URL)
		}
		if r.Latency != "" {
			fmt.Printf(" [%s]", r.Latency)
		}
		if r.Error != "" {
			fmt.Printf("\n    Error: %s", r.Error)
		}
		fmt.Println()
	}

	fmt.Println(strings.Repeat("─", 60))
	if allHealthy {
		fmt.Println("  All services healthy")
	} else {
		fmt.Println("  WARNING: Some services are unhealthy")
	}
	fmt.Println()
}
