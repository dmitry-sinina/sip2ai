package rtp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"time"

	"sip2ai/internal/ai"
	"sip2ai/internal/audio"
	"sip2ai/internal/config"
	"sip2ai/internal/metrics"
)

const levelTrace = slog.Level(-8)

// ConnectWithRetry connects provider to the AI backend, retrying on failure.
// It is called before Answer() so the call is only accepted once AI is ready.
func ConnectWithRetry(ctx context.Context, provider ai.AIProvider, cfg config.AIConfig, logger *slog.Logger) error {
	timeout := time.Duration(cfg.ConnectTimeoutMs) * time.Millisecond
	delay := time.Duration(cfg.ReconnectDelayMs) * time.Millisecond
	for attempt := 0; attempt <= cfg.ReconnectRetries; attempt++ {
		connectCtx, cancel := context.WithTimeout(ctx, timeout)
		err := provider.Connect(connectCtx, cfg.Codec, cfg.CodecRate)
		cancel()
		if err == nil {
			return nil
		} else if attempt == cfg.ReconnectRetries {
			return fmt.Errorf("AI connect after %d retries: %w", attempt, err)
		} else {
			logger.Warn("AI connect failed, retrying", "err", err, "attempt", attempt)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	return fmt.Errorf("AI connect: exhausted retries")
}

// CallSession manages a single active call: three goroutines for uplink
// (SIP->AI), downlink (AI->SIP), and health monitoring.
type CallSession struct {
	ctx    context.Context
	cancel context.CancelFunc

	encodedReader io.Reader // diago AudioReader: raw G.711 frames
	audioWriter   io.Writer // diago AudioWriter: accepts G.711 frames

	ai         ai.AIProvider
	adapter    *audio.AudioAdapter
	logger     *slog.Logger
	cfg        config.AIConfig
	metrics    *metrics.Recorder
	transferCh <-chan ai.TransferRequest
	transfer   *ai.TransferRequest
	frameBytes int // size of one 20ms RTP frame for the negotiated codec
}

// NewCallSession creates a CallSession. The provider must already be connected
// (via ConnectWithRetry) before Run() is called.
func NewCallSession(
	ctx context.Context,
	cancel context.CancelFunc,
	encodedReader io.Reader,
	audioWriter io.Writer,
	provider ai.AIProvider,
	logger *slog.Logger,
	cfg config.AIConfig,
	rec *metrics.Recorder,
) *CallSession {
	frameBytes := audio.FrameBytesFor(cfg.Codec, cfg.CodecRate)
	s := &CallSession{
		ctx:           ctx,
		cancel:        cancel,
		encodedReader: encodedReader,
		audioWriter:   audioWriter,
		ai:            provider,
		adapter:       audio.NewAudioAdapter(logger, frameBytes),
		logger:        logger,
		cfg:           cfg,
		metrics:       rec,
		frameBytes:    frameBytes,
	}
	if t, ok := provider.(ai.Transferable); ok {
		s.transferCh = t.TransferCh()
	}
	if i, ok := provider.(ai.Interruptible); ok {
		i.SetInterruptHandler(func() int {
			return s.adapter.Drain()
		})
	}
	return s
}

// Run starts the goroutines and blocks until the call ends.
// Returns a non-nil TransferRequest if the AI triggered a call transfer.
func (s *CallSession) Run() *ai.TransferRequest {
	defer s.ai.Close()
	defer s.adapter.Close()

	var wg sync.WaitGroup
	n := 3
	if s.transferCh != nil {
		n = 4
	}
	wg.Add(n)
	go func() { defer wg.Done(); s.runUplink() }()
	go func() { defer wg.Done(); s.runDownlink() }()
	go func() { defer wg.Done(); s.runHealthMonitor() }()
	if s.transferCh != nil {
		go func() { defer wg.Done(); s.runTransferWatcher() }()
	}
	wg.Wait()
	return s.transfer
}

// runUplink reads G.711 frames from RTP and forwards them to the AI.
// When the SIP phone uses silence suppression (no RTP during silence),
// synthetic silence frames are sent to keep the AI's VAD fed.
func (s *CallSession) runUplink() {
	// Channel receives RTP frames from a blocking reader goroutine.
	type rtpFrame struct {
		data []byte
		err  error
	}
	frameCh := make(chan rtpFrame, 1)
	go func() {
		buf := make([]byte, 1500)
		for {
			n, err := s.encodedReader.Read(buf)
			if err != nil {
				frameCh <- rtpFrame{err: err}
				return
			}
			if n > 0 {
				frame := make([]byte, n)
				copy(frame, buf[:n])
				frameCh <- rtpFrame{data: frame}
			}
		}
	}()

	// Per-codec silence frame, sized to one 20ms tick.
	silence := makeSilence(s.cfg.Codec, s.frameBytes)
	isL16 := s.cfg.Codec == "L16"

	// 20ms frame interval; send silence if no RTP frame arrives within 40ms.
	const silenceTimeout = 40 * time.Millisecond
	timer := time.NewTimer(silenceTimeout)
	defer timer.Stop()

	for {
		timer.Reset(silenceTimeout)
		select {
		case <-s.ctx.Done():
			return
		case f := <-frameCh:
			if f.err != nil {
				if f.err != io.EOF && s.ctx.Err() == nil {
					s.logger.Error("uplink read error", "err", f.err)
				}
				s.cancel()
				return
			}
			// RTP L16 is big-endian (RFC 3551 §4.5.11); AI backends expect
			// little-endian PCM. Swap in place — frameCh data is our copy.
			if isL16 {
				audio.SwapPCM16(f.data)
			}
			if s.cfg.LogMedia {
				s.logger.Log(context.Background(), levelTrace, "uplink: sending frame to AI", "codec", s.cfg.Codec, "bytes", len(f.data))
			}
			if err := s.ai.SendAudio(f.data); err != nil {
				if s.ctx.Err() == nil {
					s.logger.Error("uplink SendAudio error", "err", err)
				}
				s.cancel()
				return
			}
			s.metrics.AudioFrameSent()
		case <-timer.C:
			// Silence suppression: phone stopped sending RTP. Feed silence
			// to the AI so its VAD can detect end-of-speech. Silence in
			// L16 is all-zero bytes, identical in BE and LE — no swap.
			if err := s.ai.SendAudio(silence); err != nil {
				if s.ctx.Err() == nil {
					s.logger.Error("uplink SendAudio error", "err", err)
				}
				s.cancel()
				return
			}
		}
	}
}

// runDownlink pulls G.711 from the AI and writes it to RTP.
// Diago's RTPPacketWriter.Write is internally clock-gated at 20ms,
// so we use the blocking adapter.Read and let diago control the pacing.
func (s *CallSession) runDownlink() {
	// Debug: dump raw audio to files for analysis.
	// Play with: sox -t raw -r 8000 -e u-law -c 1 ai_rx.ulaw ai_rx.wav
	var rxDump, rtpDump *os.File
	if s.cfg.DumpAudio {
		var err error
		rxDump, err = os.Create("ai_rx.ulaw")
		if err == nil {
			defer rxDump.Close()
		}
		rtpDump, err = os.Create("rtp_tx.ulaw")
		if err == nil {
			defer rtpDump.Close()
		}
	}

	isL16 := s.cfg.Codec == "L16"

	// Inner goroutine: AI recv -> adapter
	go func() {
		for {
			data, err := s.ai.RecvAudio(s.ctx)
			if err != nil {
				if err != io.EOF && s.ctx.Err() == nil {
					s.logger.Error("downlink RecvAudio error", "err", err)
				}
				s.adapter.Close()
				return
			}
			s.metrics.AudioFrameReceived()
			// AI backends emit little-endian PCM; RTP L16 is big-endian.
			if isL16 {
				audio.SwapPCM16(data)
			}
			if s.cfg.LogMedia {
				s.logger.Log(context.Background(), levelTrace, "downlink: AI audio received", "codec", s.cfg.Codec, "bytes", len(data))
			}
			if rxDump != nil {
				rxDump.Write(data) //nolint:errcheck
			}
			if _, err := s.adapter.Write(data); err != nil {
				return
			}
		}
	}()

	// Per-codec silence frame, sized to one 20ms tick.
	silence := makeSilence(s.cfg.Codec, s.frameBytes)

	// Outer loop: write one frame per diago's 20ms clock tick.
	// TryRead returns audio if available, otherwise send silence to keep
	// the RTP stream continuous (prevents SIP phone timeout).
	// Diago's AudioWriter.Write blocks until the next 20ms tick internally.
	frameBuf := make([]byte, s.frameBytes)
	for {
		n, err := s.adapter.TryRead(frameBuf)
		if err != nil {
			// io.EOF: adapter closed, AI recv ended.
			s.cancel()
			return
		}
		var frame []byte
		if n > 0 {
			frame = frameBuf[:n]
			if rtpDump != nil {
				rtpDump.Write(frame) //nolint:errcheck
			}
		} else {
			frame = silence
		}
		if s.cfg.LogMedia {
			s.logger.Log(context.Background(), levelTrace, "downlink: sending frame to RTP", "codec", s.cfg.Codec, "bytes", len(frame))
		}
		if _, err := s.audioWriter.Write(frame); err != nil {
			if s.ctx.Err() == nil {
				s.logger.Error("downlink audio write error", "err", err)
			}
			s.cancel()
			return
		}
	}
}

// makeSilence returns a frameBytes-sized buffer filled with the codec's
// silence pattern (0xFF for μ-law, 0xD5 for A-law, 0x00 for L16).
func makeSilence(codec string, frameBytes int) []byte {
	b := make([]byte, frameBytes)
	fill := audio.SilenceByte(codec)
	if fill != 0 {
		for i := range b {
			b[i] = fill
		}
	}
	return b
}

// runHealthMonitor periodically pings the AI provider and triggers reconnect
// on failure.
func (s *CallSession) runHealthMonitor() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	retries := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			if err := s.ai.Ping(s.ctx); err != nil {
				retries++
				s.logger.Warn("AI ping failed", "err", err, "retries", retries)
				if retries > s.cfg.ReconnectRetries {
					s.logger.Error("AI ping: max retries exceeded, terminating call")
					s.cancel()
					return
				}
				s.ai.Close()
				if err := ConnectWithRetry(s.ctx, s.ai, s.cfg, s.logger); err != nil {
					s.logger.Error("AI reconnect failed", "err", err)
					s.cancel()
					return
				}
				retries = 0
			} else {
				retries = 0
			}
		}
	}
}

// runTransferWatcher listens for AI-triggered call transfers.
func (s *CallSession) runTransferWatcher() {
	select {
	case <-s.ctx.Done():
		return
	case req := <-s.transferCh:
		s.logger.Info("transfer requested", "destination", req.Destination)
		s.transfer = &req
		// Give the AI a moment to say "transferring you now" before we tear down.
		select {
		case <-s.ctx.Done():
		case <-time.After(3 * time.Second):
		}
		s.cancel()
	}
}
