package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wso2/amp-go/amp"
)

var (
	traceLimit  int
	traceFormat string
	traceSince  time.Duration
	traceToken  string
)

var traceCmd = &cobra.Command{
	Use:   "trace",
	Short: "Print recent traces for this agent from agent-manager-observer",
	Long: `Query agent-manager-observer for recent traces belonging to this agent.
Validates that spans are reaching OpenSearch.

Requires AMP_TRACE_OBSERVER_URL, AMP_ORG_NAME, AMP_PROJECT_NAME,
AMP_AGENT_NAME and AMP_ENVIRONMENT. The observer expects a bearer token with
trace read access: pass one with --token, otherwise the agent JWT is used when
the control plane is configured.

Example:
  amp-cli trace --n 10 --format table
  amp-cli trace --since 1h --format json`,
	RunE: runTrace,
}

func init() {
	traceCmd.Flags().IntVarP(&traceLimit, "n", "n", 10, "Number of traces to retrieve")
	traceCmd.Flags().StringVar(&traceFormat, "format", "table", "Output format: table or json")
	traceCmd.Flags().DurationVar(&traceSince, "since", 24*time.Hour, "How far back to look for traces")
	traceCmd.Flags().StringVar(&traceToken, "token", os.Getenv("AMP_TRACE_OBSERVER_TOKEN"), "Bearer token for agent-manager-observer")
}

// traceOverview matches the agent-manager-observer TraceOverviewResponse.traces[].
type traceOverview struct {
	TraceID      string `json:"traceId"`
	RootSpanName string `json:"rootSpanName"`
	RootSpanKind string `json:"rootSpanKind"`
	StartTime    string `json:"startTime"`
	EndTime      string `json:"endTime"`
	DurationNs   int64  `json:"durationInNanos"`
	SpanCount    int    `json:"spanCount"`
	TokenUsage   struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
		TotalTokens  int `json:"totalTokens"`
	} `json:"tokenUsage"`
	Status struct {
		ErrorCount int `json:"errorCount"`
	} `json:"status"`
	Input  string `json:"input"`
	Output string `json:"output"`
}

type tracesResponse struct {
	Traces     []traceOverview `json:"traces"`
	TotalCount int             `json:"totalCount"`
}

func runTrace(cmd *cobra.Command, args []string) error {
	c, err := initSDKClient()
	if err != nil {
		return err
	}

	return showRemoteTraces(c, traceLimit)
}

func showRemoteTraces(c *amp.AMPClient, limit int) error {
	cfg := c.Config()

	endpoint := cfg.TracesEndpoint()
	if endpoint == "" {
		return fmt.Errorf("AMP_TRACE_OBSERVER_URL is not set")
	}

	// The observer scopes every trace query by org, project, agent and
	// environment over a time window.
	scope := []struct{ param, value, env string }{
		{"organization", cfg.OrgName, "AMP_ORG_NAME"},
		{"project", cfg.ProjectName, "AMP_PROJECT_NAME"},
		{"agent", cfg.AgentName, "AMP_AGENT_NAME"},
		{"environment", cfg.Environment, "AMP_ENVIRONMENT"},
	}
	params := url.Values{}
	for _, sc := range scope {
		if sc.value == "" {
			return fmt.Errorf("%s is required to query traces", sc.env)
		}
		params.Set(sc.param, sc.value)
	}
	now := time.Now().UTC()
	params.Set("startTime", now.Add(-traceSince).Format(time.RFC3339))
	params.Set("endTime", now.Format(time.RFC3339))
	params.Set("limit", strconv.Itoa(limit))
	params.Set("sortOrder", "desc")

	fullURL := endpoint + "?" + params.Encode()

	httpClient := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequest(http.MethodGet, fullURL, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	token := traceToken
	if token == "" && c.Token != nil {
		token = c.Token.Token()
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var tr tracesResponse
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	if traceFormat == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(tr)
	}

	if len(tr.Traces) == 0 {
		fmt.Printf("No traces found for agent '%s'.\n", cfg.AgentName)
		fmt.Println("(Spans may take up to 10s to appear after export)")
		return nil
	}

	fmt.Printf("Traces for agent '%s' (showing %d of %d):\n\n",
		cfg.AgentName, len(tr.Traces), tr.TotalCount)

	fmt.Printf("%-4s %-38s %-10s %-8s %-12s %-10s %s\n",
		"#", "Trace ID", "Kind", "Spans", "Tokens", "Duration", "Input")
	fmt.Println(strings.Repeat("─", 110))

	for i, t := range tr.Traces {
		durationMs := t.DurationNs / 1_000_000
		inputPreview := truncate(t.Input, 40)

		fmt.Printf("%-4d %-38s %-10s %-8d %-12s %-10s %s\n",
			i+1,
			t.TraceID,
			t.RootSpanKind,
			t.SpanCount,
			fmt.Sprintf("%di %do", t.TokenUsage.InputTokens, t.TokenUsage.OutputTokens),
			fmt.Sprintf("%dms", durationMs),
			inputPreview,
		)
	}

	return nil
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > maxLen {
		return s[:maxLen-3] + "..."
	}
	return s
}
