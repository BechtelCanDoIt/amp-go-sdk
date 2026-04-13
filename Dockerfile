# Multi-stage build for amp-go binaries.
# Produces statically-linked Go binaries suitable for scratch/distroless containers.
#
# Build targets:
#   docker build --target cli -t amp-cli .
#   docker build --target chatbot -t amp-chatbot .

# --- Stage 1: Build ---
FROM golang:1.22-bookworm AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build both binaries — static linking for scratch container.
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/amp-cli ./cmd/amp-cli/
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/chatbot ./examples/chatbot/

# --- Stage 2a: CLI image ---
FROM gcr.io/distroless/static-debian12:nonroot AS cli

COPY --from=builder /out/amp-cli /usr/local/bin/amp-cli
ENTRYPOINT ["amp-cli"]

# --- Stage 2b: Chatbot image ---
FROM gcr.io/distroless/static-debian12:nonroot AS chatbot

COPY --from=builder /out/chatbot /usr/local/bin/chatbot
EXPOSE 8080
ENTRYPOINT ["chatbot"]
