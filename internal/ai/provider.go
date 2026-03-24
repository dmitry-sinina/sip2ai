package ai

import (
	"context"
	"fmt"
	"log/slog"

	"sip2ai/internal/config"
)

// AIProvider is the interface that all AI voice backends must implement.
type AIProvider interface {
	// Connect establishes the connection to the AI backend.
	Connect(ctx context.Context) error
	// SendAudio accepts a raw G.711 µ-law encoded frame (160 bytes).
	// Each provider decodes/resamples internally as needed.
	SendAudio(frame []byte) error
	// RecvAudio returns variable-length G.711 µ-law bytes at 8 kHz.
	// Each provider encodes its native output to G.711 internally.
	// Returns io.EOF on graceful AI session end.
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
func New(cfg *config.Config, logger *slog.Logger) (AIProvider, error) {
	logMedia := cfg.AI.LogMedia
	switch ProviderType(cfg.AI.Provider) {
	case ProviderOpenAI:
		return newOpenAIClient(&cfg.OpenAI, cfg.Transfers, logger, logMedia), nil
	case ProviderDeepgram:
		return newDeepgramClient(&cfg.Deepgram, logger, logMedia), nil
	case ProviderGemini:
		return newGeminiClient(&cfg.Gemini, logger, logMedia), nil
	default:
		return nil, fmt.Errorf("unknown AI provider: %q", cfg.AI.Provider)
	}
}
