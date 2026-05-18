package ai

import (
	"context"
	"fmt"
	"log/slog"

	"sip2ai/internal/config"
	"sip2ai/internal/metrics"
)

// AIProvider is the interface that all AI voice backends must implement.
type AIProvider interface {
	// Connect establishes the connection to the AI backend.
	// codec is the negotiated SIP codec name ("PCMU" or "PCMA").
	Connect(ctx context.Context, codec string) error
	// SendAudio accepts a raw G.711 encoded frame (160 bytes, ulaw or alaw
	// depending on the codec passed to Connect).
	SendAudio(frame []byte) error
	// RecvAudio returns variable-length G.711 bytes at 8 kHz (ulaw or alaw
	// matching the codec passed to Connect).
	RecvAudio(ctx context.Context) ([]byte, error)
	// Ping checks whether the connection is still alive.
	Ping(ctx context.Context) error
	// Close tears down the connection.
	Close() error
}

// TransferRequest signals that the AI decided to transfer the call.
type TransferRequest struct {
	Destination string // E.164 phone number
}

// Transferable is an optional interface for providers that support
// AI-triggered call transfers via function calling.
type Transferable interface {
	TransferCh() <-chan TransferRequest
}

// ProviderType identifies an AI backend.
type ProviderType string

const (
	ProviderOpenAI   ProviderType = "openai"
	ProviderDeepgram ProviderType = "deepgram"
	ProviderGemini   ProviderType = "gemini"
)

// New constructs the AIProvider specified in cfg.AI.Provider.
// logger is used for provider-specific diagnostic messages.
func New(cfg *config.Config, logger *slog.Logger, rec *metrics.Recorder) (AIProvider, error) {
	logMedia := cfg.AI.LogMedia
	switch ProviderType(cfg.AI.Provider) {
	case ProviderOpenAI:
		return newOpenAIClient(&cfg.OpenAI, cfg.Transfers, logger, logMedia, rec), nil
	case ProviderDeepgram:
		return newDeepgramClient(&cfg.Deepgram, logger, logMedia, rec), nil
	case ProviderGemini:
		return newGeminiClient(&cfg.Gemini, logger, logMedia, rec), nil
	default:
		return nil, fmt.Errorf("unknown AI provider: %q", cfg.AI.Provider)
	}
}
