package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// chatRequest is the JSON body sent to the agent's /chat endpoint.
type chatRequest struct {
	Message string `json:"message"`
}

// chatResponse is the JSON body returned by the agent's /chat endpoint.
type chatResponse struct {
	Reply          string `json:"reply"`
	ConversationID string `json:"conversation_id"`
	Tokens         struct {
		Prompt     int `json:"prompt"`
		Completion int `json:"completion"`
	} `json:"tokens"`
	LatencyMs int `json:"latency_ms"`
}

var chatCmd = &cobra.Command{
	Use:   "chat",
	Short: "Interactive multi-turn conversation with an agent",
	Long: `Start an interactive REPL for multi-turn conversation with an AMP agent.
Persists X-Conversation-ID across turns. Type 'exit' to quit, 'traces' to
show the last 5 spans.

Example:
  amp-cli chat --agent-url http://chatbot:8080`,
	RunE: runChat,
}

func runChat(cmd *cobra.Command, args []string) error {
	conversationID := uuid.New().String()
	httpClient := &http.Client{Timeout: 120 * time.Second}

	fmt.Println("AMP Agent CLI — type 'exit' to quit, 'traces' to show last 5 spans")
	fmt.Printf("Agent: %s\n", agentURL)
	fmt.Printf("Conversation: %s\n\n", conversationID)

	scanner := bufio.NewScanner(os.Stdin)

	// Track exchanges for local trace display.
	type exchange struct {
		latencyMs      int
		promptTokens   int
		completeTokens int
	}
	var exchanges []exchange

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			break
		}

		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		switch strings.ToLower(input) {
		case "exit", "quit":
			fmt.Println("Goodbye!")
			return nil

		case "traces":
			// Show local trace summary.
			if len(exchanges) == 0 {
				fmt.Println("No exchanges yet.")
				continue
			}
			fmt.Println()
			fmt.Printf("%-4s %-8s %-12s %-10s %-10s\n", "#", "Type", "Model", "Tokens", "Latency")
			fmt.Println(strings.Repeat("─", 50))
			for i, ex := range exchanges {
				spanNum := i*2 + 1
				fmt.Printf("%-4d %-8s %-12s %-10s %-10s\n",
					spanNum, "agent", "-", "-",
					fmt.Sprintf("%dms", ex.latencyMs))
				fmt.Printf("%-4d %-8s %-12s %-10s %-10s\n",
					spanNum+1, "llm", "-",
					fmt.Sprintf("%dp %dc", ex.promptTokens, ex.completeTokens),
					"-")
			}
			fmt.Println()

			// Also try server-side trace lookup if SDK is available.
			c, err := initSDKClient()
			if err == nil {
				showRemoteTraces(c, 5)
			}
			continue

		case "health":
			c, err := initSDKClient()
			if err != nil {
				fmt.Printf("SDK init failed: %v\n", err)
				continue
			}
			runHealthCheck(c)
			continue
		}

		// Send message to agent.
		body, _ := json.Marshal(chatRequest{Message: input})

		req, err := http.NewRequest(http.MethodPost, agentURL+"/chat", bytes.NewReader(body))
		if err != nil {
			fmt.Printf("Error building request: %v\n", err)
			continue
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Conversation-ID", conversationID)
		if userToken != "" {
			req.Header.Set("X-User-Token", userToken)
		}

		start := time.Now()
		resp, err := httpClient.Do(req)
		latency := time.Since(start)

		if err != nil {
			fmt.Printf("Error: %v\n", err)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			fmt.Printf("Error %d: %s\n", resp.StatusCode, string(respBody))
			continue
		}

		// Update conversation ID if server returned a different one.
		if serverConvID := resp.Header.Get("X-Conversation-ID"); serverConvID != "" {
			conversationID = serverConvID
		}

		var cr chatResponse
		if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
			fmt.Printf("Error decoding response: %v\n", err)
			continue
		}

		// Display response with metadata.
		fmt.Printf("\nAgent: %s", cr.Reply)
		fmt.Printf(" [latency: %s | tokens: %dp %dc]\n\n",
			latency.Round(time.Millisecond),
			cr.Tokens.Prompt,
			cr.Tokens.Completion,
		)

		exchanges = append(exchanges, exchange{
			latencyMs:      int(latency.Milliseconds()),
			promptTokens:   cr.Tokens.Prompt,
			completeTokens: cr.Tokens.Completion,
		})
	}

	return scanner.Err()
}
