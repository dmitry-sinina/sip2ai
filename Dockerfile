FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG BUILD_TIME
RUN BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION} -X main.buildTime=${BUILD_TIME}" -o /sip2ai ./cmd/sip2ai && \
    cd sip2openai && \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o /sip2openai ./cmd/sip2openai

FROM gcr.io/distroless/static-debian12
COPY --from=builder /sip2ai /sip2ai
COPY --from=builder /sip2openai /sip2openai
COPY config.yaml.distr /etc/sip2ai/config.yaml.distr
COPY sip2openai/config.yaml.distr /etc/sip2openai/config.yaml.distr
WORKDIR /etc/sip2ai
# Default entrypoint is sip2ai; override the command with /sip2openai to run the
# signaling-only OpenAI gateway from the same image.
ENTRYPOINT ["/sip2ai"]
