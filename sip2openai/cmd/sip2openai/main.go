// Command sip2openai is a signaling-only gateway between SIP and OpenAI's
// Realtime WebRTC API: it relays SDP and controls calls, while media flows
// directly between the caller and OpenAI.
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/emiago/sipgo/sip"

	"sip2openai/internal/config"
	"sip2openai/internal/openai"
	sipsrv "sip2openai/internal/sip"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.Log)
	slog.SetDefault(logger)

	// Raise sipgo's UDP message cap so large WebRTC SDP answers fit the 200 OK.
	if cfg.SIP.UDPMTU > 0 {
		sip.UDPMTUSize = cfg.SIP.UDPMTU
		logger.Info("UDP message cap set", "udp_mtu", cfg.SIP.UDPMTU, "tcp_enabled", cfg.SIP.EnableTCP)
	}

	if cfg.OpenAI.APIKey == "" {
		logger.Error("OpenAI API key missing (set openai.api_key or OPENAI_API_KEY)")
		os.Exit(1)
	}
	oai, err := openai.New(cfg.OpenAI.APIKey, cfg.OpenAI.Model, cfg.OpenAI.BaseURL, cfg.OpenAI.Proxy)
	if err != nil {
		logger.Error("create OpenAI client", "err", err)
		os.Exit(1)
	}

	srv, err := sipsrv.New(cfg.SIP, cfg.OpenAI, cfg.Transfers, oai, logger)
	if err != nil {
		logger.Error("create SIP server", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("sip2openai starting", "model", cfg.OpenAI.Model, "bind", cfg.SIP.BindHost, "port", cfg.SIP.BindPort)
	if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
		logger.Error("server stopped", "err", err)
		os.Exit(1)
	}
	logger.Info("sip2openai stopped")
}

func newLogger(cfg config.LogConfig) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(cfg.Level) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if strings.ToLower(cfg.Format) == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	return slog.New(h)
}
