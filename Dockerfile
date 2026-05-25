FROM golang:1.24 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG BUILD_TIME
RUN BUILD_TIME="${BUILD_TIME:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION} -X main.buildTime=${BUILD_TIME}" -o /sip2ai ./cmd/sip2ai

FROM gcr.io/distroless/static-debian12
COPY --from=builder /sip2ai /sip2ai
COPY config.yaml.distr /etc/sip2ai/config.yaml.distr
WORKDIR /etc/sip2ai
ENTRYPOINT ["/sip2ai"]
