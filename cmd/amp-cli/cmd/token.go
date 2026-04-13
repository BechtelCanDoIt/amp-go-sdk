package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Fetch and print a fresh agent JWT",
	Long: `Acquire a fresh agent JWT from the AMP API and print it. Useful for
debugging token acquisition, inspecting claims, and verifying that the
AMP API is reachable with the configured API key.

Example:
  amp-cli token
  amp-cli token --json`,
	RunE: runToken,
}

var tokenOutputJSON bool

func init() {
	tokenCmd.Flags().BoolVar(&tokenOutputJSON, "json", false, "Output token details as JSON")
}

func runToken(cmd *cobra.Command, args []string) error {
	c, err := initSDKClient()
	if err != nil {
		return err
	}

	token := c.Token.Token()
	claims := c.Token.Claims()
	expiresAt := c.Token.ExpiresAt()
	issuedAt := c.Token.IssuedAt()

	if tokenOutputJSON {
		output := map[string]any{
			"token":           token,
			"component_uid":   claims.ComponentUID,
			"environment_uid": claims.EnvironmentUID,
			"project_uid":     claims.ProjectUID,
			"expires_at":      expiresAt.Format(time.RFC3339),
			"issued_at":       issuedAt.Format(time.RFC3339),
			"ttl_seconds":     int(time.Until(expiresAt).Seconds()),
			"expired":         c.Token.IsExpired(),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	fmt.Printf("Agent JWT for '%s':\n\n", c.Config().AgentName)
	fmt.Printf("Token:           %s...%s\n", token[:min(20, len(token))], token[max(0, len(token)-10):])
	fmt.Printf("Component UID:   %s\n", claims.ComponentUID)
	fmt.Printf("Environment UID: %s\n", claims.EnvironmentUID)
	fmt.Printf("Project UID:     %s\n", claims.ProjectUID)
	fmt.Printf("Issued At:       %s\n", issuedAt.Format(time.RFC3339))
	fmt.Printf("Expires At:      %s\n", expiresAt.Format(time.RFC3339))
	fmt.Printf("TTL:             %s\n", time.Until(expiresAt).Round(time.Second))
	fmt.Printf("Expired:         %v\n", c.Token.IsExpired())

	if debug {
		fmt.Printf("\nFull token:\n%s\n", token)
	}

	return nil
}
