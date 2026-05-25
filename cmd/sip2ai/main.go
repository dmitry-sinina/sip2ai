package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/emiago/sipgo/sip"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"sip2ai/internal/ai"
	"sip2ai/internal/config"
	"sip2ai/internal/metrics"
	sipserver "sip2ai/internal/sip"
	"sip2ai/internal/siplog"
)

// LevelTrace is below slog.LevelDebug, used for audio frame and payload logs.
const LevelTrace = slog.Level(-8)

// version and buildTime are set via -ldflags at build time.
var (
	version   = "dev"
	buildTime = "unknown"
)

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
	sip.SetDefaultLogger(sipLogger)

	// debug: print full SIP messages at the transport layer.
	// trace: additionally log transaction FSM state transitions.
	if sipLvl <= slog.LevelDebug {
		sip.SIPDebug = true
	}
	if sipLvl <= LevelTrace {
		sip.TransactionFSMDebug = true
	}

	providerLoggers := map[string]*slog.Logger{
		"openai":   newLogger(openAILvl),
		"deepgram": newLogger(deepgramLvl),
		"gemini":   newLogger(geminiLvl),
	}

	// Prometheus metrics.
	var rec *metrics.Recorder
	if cfg.Metrics.Enabled {
		registry := prometheus.NewRegistry()
		registerer := prometheus.Registerer(registry)
		if len(cfg.Metrics.Labels) > 0 {
			registerer = prometheus.WrapRegistererWith(prometheus.Labels(cfg.Metrics.Labels), registry)
		}
		rec = metrics.NewRecorder(registerer, cfg.Log.Media)

		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
		go func() {
			slog.Info("metrics server starting", "listen", cfg.Metrics.Listen)
			if err := http.ListenAndServe(cfg.Metrics.Listen, mux); err != nil {
				slog.Error("metrics server failed", "err", err)
			}
		}()
	}

	aiFactory := func(cid string, callCfg *config.Config) ai.AIProvider {
		providerLogger := providerLoggers[callCfg.AI.Provider]
		if providerLogger == nil {
			providerLogger = newLogger(minLvl)
		}
		logger := providerLogger.With("cid", cid)
		p, err := ai.New(callCfg, logger, rec)
		if err != nil {
			slog.Error("AI provider creation failed", "err", err)
			os.Exit(1)
		}
		return p
	}

	server, err := sipserver.NewServer(cfg, aiFactory, sipLogger, version, rec)
	if err != nil {
		slog.Error("SIP server creation failed", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	slog.Info("sip2ai starting", "version", version, "build_time", buildTime, "provider", cfg.AI.Provider, "log_level", cfg.Log.Level)
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
