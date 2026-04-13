package cmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"
)

var (
	sendMessage        string
	sendConversationID string
	sendOutputJSON     bool
)

var sendCmd = &cobra.Command{
	Use:   "send",
	Short: "Send a single message to an agent (CI-friendly)",
	Long: `Send one message to the agent and print the response. Designed for
scripted/CI usage. Exits with code 0 on success, 1 on error.

Example:
  amp-cli send --message "What can you help with?" --agent-url http://chatbot:8080
  amp-cli send -m "Hello" --json  # machine-readable output`,
	RunE: runSend,
}

func init() {
	sendCmd.Flags().StringVarP(&sendMessage, "message", "m", "", "Message to send (required)")
	sendCmd.Flags().StringVar(&sendConversationID, "conversation-id", "", "Conversation ID (generates new if empty)")
	sendCmd.Flags().BoolVar(&sendOutputJSON, "json", false, "Output full JSON response")
	sendCmd.MarkFlagRequired("message")
}

func runSend(cmd *cobra.Command, args []string) error {
	httpClient := &http.Client{Timeout: 120 * time.Second}

	body, _ := json.Marshal(chatRequest{Message: sendMessage})
	req, err := http.NewRequest(http.MethodPost, agentURL+"/chat", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if sendConversationID != "" {
		req.Header.Set("X-Conversation-ID", sendConversationID)
	}
	if userToken != "" {
		req.Header.Set("X-User-Token", userToken)
	}

	start := time.Now()
	resp, err := httpClient.Do(req)
	latency := time.Since(start)
	if err != nil {
		return fmt.Errorf("POST %s/chat: %w", agentURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var cr chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	if sendOutputJSON {
		// Full structured output for CI parsing.
		output := map[string]any{
			"reply":           cr.Reply,
			"conversation_id": cr.ConversationID,
			"tokens": map[string]int{
				"prompt":     cr.Tokens.Prompt,
				"completion": cr.Tokens.Completion,
			},
			"latency_ms":        int(latency.Milliseconds()),
			"server_latency_ms": cr.LatencyMs,
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(output)
	}

	// Human-readable output.
	fmt.Println(cr.Reply)
	if debug {
		fmt.Printf("\n--- conversation: %s | latency: %s | tokens: %dp %dc ---\n",
			cr.ConversationID, latency.Round(time.Millisecond),
			cr.Tokens.Prompt, cr.Tokens.Completion)
	}

	return nil
}
