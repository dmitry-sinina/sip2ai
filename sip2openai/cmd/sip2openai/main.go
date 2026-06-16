// Command sip2openai is a signaling-only gateway between SIP and OpenAI's
// Realtime WebRTC API: it relays SDP and controls calls, while media flows
// directly between the caller and OpenAI.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/emiago/sipgo/sip"

	"sip2openai/internal/config"
	"sip2openai/internal/openai"
	sipsrv "sip2openai/internal/sip"
	"sip2openai/internal/siplog"
)

// LevelTrace is below slog.LevelDebug; used for full SIP frames and full
// OpenAI sideband event payloads.
const LevelTrace = slog.Level(-8)

func main() {
	cfgPath := flag.String("config", "config.yaml", "path to config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		slog.Error("load config", "err", err)
		os.Exit(1)
	}

	// Resolve per-component levels; each empty value falls back to the base.
	parseLvl := func(name, val, fallback string) slog.Level {
		if val == "" {
			val = fallback
		}
		l, err := parseLevel(val)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid log level %q for %s: %v\n", val, name, err)
			os.Exit(1)
		}
		return l
	}
	baseLvl := parseLvl("level", cfg.Log.Level, "info")
	sipLvl := parseLvl("sip", cfg.Log.SIP, cfg.Log.Level)
	openAILvl := parseLvl("openai", cfg.Log.OpenAI, cfg.Log.Level)

	newLogger := func(level slog.Level) *slog.Logger {
		var h slog.Handler
		if strings.ToLower(cfg.Log.Format) == "json" {
			h = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level})
		} else {
			h = siplog.New(os.Stderr, level)
		}
		return slog.New(h)
	}

	// Global minimum so the default logger never filters out a more verbose
	// per-component logger.
	minLvl := baseLvl
	for _, l := range []slog.Level{sipLvl, openAILvl} {
		if l < minLvl {
			minLvl = l
		}
	}

	logger := newLogger(minLvl)
	slog.SetDefault(logger)
	oaiLogger := newLogger(openAILvl)

	// Wire slog into sipgo and enable wire-level tracing per the SIP level.
	sip.SetDefaultLogger(newLogger(sipLvl))
	if sipLvl <= slog.LevelDebug {
		sip.SIPDebug = true // dump full SIP messages at the transport layer
	}
	if sipLvl <= LevelTrace {
		sip.TransactionFSMDebug = true // log transaction FSM state transitions
	}

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

	srv, err := sipsrv.New(cfg.SIP, cfg.OpenAI, cfg.Transfers, oai, logger, oaiLogger)
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

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "trace":
		return LevelTrace, nil
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown level %q (trace|debug|info|warn|error)", s)
	}
}
