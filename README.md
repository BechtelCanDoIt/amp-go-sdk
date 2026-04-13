# amp-go — Go SDK for WSO2 Agent Manager

A first-class Go SDK for the [WSO2 Agent Manager](https://github.com/wso2/agent-manager) platform, functionally equivalent to the Python `amp-instrumentation` SDK. Includes a CLI test harness and a reference multi-turn chatbot.

## Deliverables

| Package | Description |
|---|---|
| `amp/` | Core SDK: JWT lifecycle, OTLP tracing, conversation management, HTTP middleware |
| `instrumentation/` | Zero-code auto-init via `import _` (Go's `sitecustomize` equivalent) |
| `cmd/amp-cli/` | CLI test harness: `chat`, `send`, `trace`, `token`, `health` subcommands |
| `examples/chatbot/` | Reference HTTP chatbot with full middleware chain and LLM integration |
| `deploy/` | Kubernetes manifests for CLI Job and chatbot Deployment |

## Quickstart

### Prerequisites

- Go 1.22+
- Access to a WSO2 Agent Manager instance (AMP API + obs-gateway)
- An LLM backend (Ollama for local dev, WSO2 AI Gateway or market LLM for prod)

### 1. Set environment variables

```bash
# Required — AMP platform
export AMP_OTEL_ENDPOINT=http://obs-gateway:22893
export AMP_AGENT_API_KEY=<your-agent-api-key>
export AMP_BASE_URL=http://amp-api:9095
export AMP_ORG_NAME=myorg
export AMP_PROJECT_NAME=myproject
export AMP_AGENT_NAME=chatbot

# LLM backend — defaults to Ollama
export LLM_API_URL=http://localhost:11434/v1
export LLM_MODEL=llama3.2

# Optional
export AMP_TRACE_CONTENT=true       # Capture prompts/completions in spans
export AMP_ENVIRONMENT=development
export AMP_DEBUG=0
```

### 2. Run the chatbot

```bash
# Local (requires Ollama running with llama3.2)
go run ./examples/chatbot/

# Test it
curl -X POST http://localhost:8080/chat \
  -H "Content-Type: application/json" \
  -d '{"message": "Hello, what can you help me with?"}'
```

### 3. Use the CLI

```bash
# Build
go build -o bin/amp-cli ./cmd/amp-cli/

# Health check
bin/amp-cli health

# Interactive chat
bin/amp-cli chat --agent-url http://localhost:8080

# Single message (CI-friendly)
bin/amp-cli send -m "Hello" --agent-url http://localhost:8080 --json

# View traces
bin/amp-cli trace --n 10

# Fetch agent JWT
bin/amp-cli token --json
```

### 4. Deploy to Kubernetes

```bash
# Build images
docker build --target chatbot -t ghcr.io/wso2/amp-chatbot:latest .
docker build --target cli -t ghcr.io/wso2/amp-cli:latest .

# Deploy
kubectl apply -f deploy/chatbot-deployment.yaml
kubectl apply -f deploy/amp-cli-job.yaml

# Interactive CLI inside the cluster
kubectl apply -f deploy/amp-cli-job.yaml  # uses the "interactive" Job
kubectl exec -it job/amp-cli-interactive -- amp-cli chat --agent-url http://chatbot:8080
```

## Architecture

```
Developer/CI                     K8s Cluster
─────────────                    ────────────────────────────────────────────
                                 ┌─────────────────┐
amp-cli chat ──HTTP POST /chat──▶│ Go Agent :8080   │
  X-Conversation-ID              │                  │
  X-User-Token                   │  ConversationMW  │
                                 │  → LeastPrivMW   │
                                 │  → chatHandler   │
                                 │    AgentSpan     │
                                 │      LLMSpan ────┼──▶ LLM Backend
                                 │                  │    (Ollama/APIM/OpenAI)
                                 └────────┬─────────┘
                                          │ OTLP (async batch)
                                          │ x-amp-api-key header
                                          ▼
                                 ┌─────────────────┐
                                 │ obs-gateway      │
                                 │ :22893           │
                                 └────────┬─────────┘
                                          ▼
                                 ┌─────────────────┐
                                 │ OTel Collector   │
                                 │ :4318            │
                                 └────────┬─────────┘
                                          ▼
                                 ┌─────────────────┐
                                 │ OpenSearch       │
                                 └────────┬─────────┘
                                          ▼
                                 ┌─────────────────┐
                                 │ AMP Console      │
                                 │ (Trace UI)       │
                                 └─────────────────┘
```

## SDK API

### Explicit init

```go
cfg := amp.FromEnv()
client, err := amp.Init(cfg)
if err != nil {
    log.Fatal(err)
}
defer client.Shutdown(context.Background())
```

### Zero-code auto-init

```go
import _ "github.com/wso2/amp-go/instrumentation"

// That's it. The SDK initializes in init() before main().
// Access via instrumentation.Client if needed.
```

### Span instrumentation

```go
// Agent span — root span for a conversation turn
client.AgentSpan(ctx, "my-agent", conversationID, inputMessages,
    func(ctx context.Context) error {
        // LLM span — child of agent span
        result, err := client.LLMSpan(ctx, amp.LLMRequest{
            Model:    "gpt-4o",
            Provider: "openai",
        }, func(ctx context.Context) (amp.LLMResult, error) {
            // Call your LLM here
            return amp.LLMResult{
                Content:          "response",
                PromptTokens:     42,
                CompletionTokens: 87,
            }, nil
        })
        return err
    },
)

// Tool span — child of agent span
client.ToolSpan(ctx, "search_docs", input,
    func(ctx context.Context) (any, error) {
        return searchDocs(input), nil
    },
)
```

### HTTP middleware

```go
mux := http.NewServeMux()

// Chain: ConversationMiddleware → LeastPrivilegeMiddleware → handler
chain := client.ConversationMiddleware()(
    client.LeastPrivilegeMiddleware([]string{"read:docs", "read:orders"})(
        myHandler,
    ),
)
mux.Handle("/chat", chain)
```

### Context values

```go
func myHandler(w http.ResponseWriter, r *http.Request) {
    convID := amp.ConversationIDFromContext(r.Context())
    scope  := amp.EffectiveScopeFromContext(r.Context())
    token  := amp.UserTokenFromContext(r.Context())
}
```

## OTEL Span Attribute Contract

These are the exact attributes `amp-trace-observer` reads. Incorrect or missing attributes cause spans to appear as "unknown" in the AMP Console.

| Attribute | Value | Span Type |
|---|---|---|
| `traceloop.span.kind` | `"agent"` \| `"llm"` \| `"tool"` | All (primary classifier) |
| `gen_ai.agent.name` | Agent name string | agent |
| `gen_ai.conversation.id` | UUID per conversation | agent |
| `gen_ai.system` | `"openai"` \| `"ollama"` etc | llm |
| `gen_ai.request.model` | e.g. `"gpt-4o"` | llm |
| `gen_ai.response.model` | Actual model used | llm |
| `gen_ai.request.temperature` | float64 | llm |
| `gen_ai.usage.input_tokens` | int (preferred) | llm |
| `gen_ai.usage.output_tokens` | int (preferred) | llm |
| `gen_ai.usage.prompt_tokens` | int (compat) | llm |
| `gen_ai.usage.completion_tokens` | int (compat) | llm |
| `gen_ai.tool.name` | Tool/function name | tool |
| `gen_ai.input.messages` | JSON (if TRACE_CONTENT) | agent/llm |
| `gen_ai.output.messages` | JSON (if TRACE_CONTENT) | agent/llm |

## Configuration Reference

| Env Var | Description | Default |
|---|---|---|
| `AMP_OTEL_ENDPOINT` | OTLP HTTP endpoint (obs-gateway) | **required** |
| `AMP_AGENT_API_KEY` | Agent API key from AMP Console | **required** |
| `AMP_BASE_URL` | AMP API base URL | **required** |
| `AMP_ORG_NAME` | Organization name | **required** |
| `AMP_PROJECT_NAME` | Project name | **required** |
| `AMP_AGENT_NAME` | Agent name | **required** |
| `AMP_TRACE_CONTENT` | Capture prompts/completions | `true` |
| `AMP_AGENT_VERSION` | Resource attribute | `""` |
| `AMP_TOKEN_REFRESH_BUFFER` | Refresh JWT before expiry | `5m` |
| `AMP_DEBUG` | Verbose logging | `0` |
| `AMP_ENVIRONMENT` | Environment name | `development` |
| `LLM_API_URL` | LLM backend URL | `http://localhost:11434/v1` |
| `LLM_API_KEY` | LLM API key | `""` |
| `LLM_MODEL` | Default model | `llama3.2` |

## Testing

```bash
# Unit tests with race detector (BRD acceptance criterion)
go test -race -v ./...

# Quick check
make test
```

## LLM Backend Configuration

### Ollama (local development)

```bash
# Install and start Ollama
ollama serve
ollama pull llama3.2

# SDK defaults point here
export LLM_API_URL=http://localhost:11434/v1
export LLM_MODEL=llama3.2
```

### WSO2 APIM / AI Gateway (dev/prod)

```bash
export LLM_API_URL=https://apim-gateway:8243/ai/v1
export LLM_API_KEY=<apim-consumer-key>
export LLM_MODEL=gpt-4o
```

### OpenAI (direct)

```bash
export LLM_API_URL=https://api.openai.com/v1
export LLM_API_KEY=sk-...
export LLM_MODEL=gpt-4o
```

## Project Structure

```
amp-go/
├── amp/                        Core SDK package
│   ├── config.go               Config struct + FromEnv()
│   ├── token.go                JWT acquire, parse, refresh goroutine
│   ├── tracer.go               OTLP setup, AgentSpan/LLMSpan/ToolSpan
│   ├── conversation.go         sync.Map + TTL eviction
│   ├── client.go               AMPClient wiring + health checks
│   ├── middleware.go            Least-privilege + conversation middleware
│   └── *_test.go               Unit tests
├── instrumentation/
│   └── auto.go                 init() — zero-code auto-init
├── cmd/amp-cli/
│   ├── main.go                 CLI entry point
│   └── cmd/                    Cobra subcommands
│       ├── root.go             Global flags + SDK init
│       ├── chat.go             Interactive REPL
│       ├── send.go             Single-shot message
│       ├── trace.go            Query amp-trace-observer
│       ├── token.go            Fetch agent JWT
│       └── health.go           Connectivity check
├── examples/chatbot/
│   └── main.go                 Reference HTTP chatbot
├── deploy/
│   ├── amp-cli-job.yaml        K8s Job for CLI
│   └── chatbot-deployment.yaml K8s Deployment + Service
├── Dockerfile                  Multi-stage (cli + chatbot targets)
├── Makefile                    Build, test, docker, run
├── go.mod                      Module definition
└── README.md                   This file
```
