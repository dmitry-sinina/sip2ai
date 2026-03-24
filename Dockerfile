FROM golang:1.24 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /sip2ai ./cmd/sip2ai

FROM gcr.io/distroless/static-debian12
COPY --from=builder /sip2ai /sip2ai
COPY config.yaml.distr /etc/sip2ai/config.yaml.distr
WORKDIR /etc/sip2ai
ENTRYPOINT ["/sip2ai"]
