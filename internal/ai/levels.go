package ai

import "log/slog"

// LevelTrace is a custom slog level below Debug, used for high-frequency
// diagnostic messages (audio frame logs, full protocol payloads).
const LevelTrace = slog.Level(-8)
