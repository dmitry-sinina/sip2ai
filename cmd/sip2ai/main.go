package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	sipstack "github.com/emiago/sipgo/sip"
	"sip2ai/internal/ai"
	"sip2ai/internal/config"
	"sip2ai/internal/sip"
	"sip2ai/internal/siplog"
)

// LevelTrace is below slog.LevelDebug, used for audio frame and payload logs.
const LevelTrace = slog.Level(-8)

// version is set via -ldflags at build time.
var version = "dev"

func main() {
	logFormat        := flag.String("log-format",         "",     "Log format (text|json), overrides config")
	logLevel         := flag.String("log-level",          "",     "Default log level, overrides config (trace|debug|info|warn|error)")
	sipLogLevel      := flag.String("sip-log-level",      "",     "SIP stack log level, overrides config")
	openAILogLevel   := flag.String("openai-log-level",   "",     "OpenAI provider log level, overrides config")
	deepgramLogLevel := flag.String("deepgram-log-level", "",     "Deepgram provider log level, overrides config")
	geminiLogLevel   := flag.String("gemini-log-level",   "",     "Gemini provider log level, overrides config")
	logMedia         := flag.Bool("log-media",            false,  "Log per-frame audio send/receive (high volume, expensive)")
	flag.Parse()

	// Load config first, then apply CLI overrides.
	cfg, err := config.Load("config.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "config load failed: %v\n", err)
		os.Exit(1)
	}

	// CLI flags override YAML values when explicitly set.
	if *logFormat != "" {
		cfg.Log.Format = *logFormat
	}
	if *logLevel != "" {
		cfg.Log.Level = *logLevel
	}
	if *sipLogLevel != "" {
		cfg.Log.SIP = *sipLogLevel
	}
	if *openAILogLevel != "" {
		cfg.Log.OpenAI = *openAILogLevel
	}
	if *deepgramLogLevel != "" {
		cfg.Log.Deepgram = *deepgramLogLevel
	}
	if *geminiLogLevel != "" {
		cfg.Log.Gemini = *geminiLogLevel
	}
	if *logMedia {
		cfg.Log.Media = true
	}
	cfg.AI.LogMedia = cfg.Log.Media

	// Parse resolved log levels. Per-component defaults to base level.
	parseLvl := func(name, val, defaultVal string) slog.Level {
		if val == "" {
			val = defaultVal
		}
		l, err := parseLevel(val)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid log level %q for %s: %v\n", val, name, err)
			os.Exit(1)
		}
		return l
	}

	baseLvl     := parseLvl("level",    cfg.Log.Level,    "warn")
	sipLvl      := parseLvl("sip",      cfg.Log.SIP,      cfg.Log.Level)
	openAILvl   := parseLvl("openai",   cfg.Log.OpenAI,   cfg.Log.Level)
	deepgramLvl := parseLvl("deepgram", cfg.Log.Deepgram,  cfg.Log.Level)
	geminiLvl   := parseLvl("gemini",   cfg.Log.Gemini,   cfg.Log.Level)

	// Global minimum: lowest level across all components.
	minLvl := baseLvl
	for _, l := range []slog.Level{sipLvl, openAILvl, deepgramLvl, geminiLvl} {
		if l < minLvl {
			minLvl = l
		}
	}

	newLogger := func(level slog.Level) *slog.Logger {
		var h slog.Handler
		switch cfg.Log.Format {
		case "json":
			h = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
		default:
			h = siplog.New(os.Stdout, level)
		}
		return slog.New(h)
	}

	// Base logger used for app-level messages.
	slog.SetDefault(newLogger(minLvl))
	sipLogger := newLogger(sipLvl)

	// Wire slog into sipgo and diago.
	sipstack.SetDefaultLogger(sipLogger)

	// debug: print full SIP messages at the transport layer.
	// trace: additionally log transaction FSM state transitions.
	if sipLvl <= slog.LevelDebug {
		sipstack.SIPDebug = true
	}
	if sipLvl <= LevelTrace {
		sipstack.TransactionFSMDebug = true
	}

	providerLoggers := map[string]*slog.Logger{
		"openai":   newLogger(openAILvl),
		"deepgram": newLogger(deepgramLvl),
		"gemini":   newLogger(geminiLvl),
	}

	aiFactory := func(cid string, callCfg *config.Config) ai.AIProvider {
		providerLogger := providerLoggers[callCfg.AI.Provider]
		if providerLogger == nil {
			providerLogger = newLogger(minLvl)
		}
		logger := providerLogger.With("cid", cid)
		p, err := ai.New(callCfg, logger)
		if err != nil {
			slog.Error("AI provider creation failed", "err", err)
			os.Exit(1)
		}
		return p
	}

	server, err := sip.NewServer(cfg, aiFactory, sipLogger, version)
	if err != nil {
		slog.Error("SIP server creation failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("sip2ai starting", "version", version, "provider", cfg.AI.Provider, "log_level", cfg.Log.Level)
	if err := server.Start(ctx); err != nil {
		slog.Error("server error", "err", err)
		os.Exit(1)
	}
}

func parseLevel(s string) (slog.Level, error) {
	switch s {
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
