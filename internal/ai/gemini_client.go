package ai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"sip2ai/internal/audio"
	"sip2ai/internal/config"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

const geminiEndpoint = "wss://generativelanguage.googleapis.com/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent"

type geminiClient struct {
	cfg      *config.GeminiConfig
	logger   *slog.Logger
	logMedia bool
	conn     *websocket.Conn
	recvCh chan []byte
	errCh  chan error
	sendMu sync.Mutex
	done   chan struct{}

	// Pre-allocated resample buffers; protected by sendMu
	pcm8buf  [audio.FrameSamples]int16
	pcm16buf [audio.FrameSamples * 2]int16

	mu       sync.Mutex
	lastRecv time.Time
}

func newGeminiClient(cfg *config.GeminiConfig, logger *slog.Logger, logMedia bool) *geminiClient {
	return &geminiClient{
		cfg:      cfg,
		logger:   logger,
		logMedia: logMedia,
		recvCh:   make(chan []byte, 64),
		errCh:    make(chan error, 4),
		done:     make(chan struct{}),
	}
}

func (c *geminiClient) Connect(ctx context.Context) error {
	wsURL := fmt.Sprintf("%s?key=%s", geminiEndpoint, c.cfg.APIKey)
	var dialOpts *websocket.DialOptions
	if c.cfg.Proxy != "" {
		proxyURL, err := url.Parse(c.cfg.Proxy)
		if err != nil {
			return fmt.Errorf("gemini proxy URL: %w", err)
		}
		dialOpts = &websocket.DialOptions{
			HTTPClient: &http.Client{
				Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
			},
		}
	}
	conn, _, err := websocket.Dial(ctx, wsURL, dialOpts)
	if err != nil {
		return fmt.Errorf("gemini dial: %w", err)
	}
	conn.SetReadLimit(-1)
	c.conn = conn

	setup := map[string]any{
		"setup": map[string]any{
			"model": c.cfg.Model,
			"generation_config": map[string]any{
				"response_modalities": []string{"AUDIO"},
			},
			"system_instruction": map[string]any{
				"parts": []map[string]any{
					{"text": c.cfg.SystemPrompt},
				},
			},
		},
	}
	if raw, err := json.Marshal(setup); err == nil {
		c.logger.Log(ctx, LevelTrace, "gemini tx setup", "payload", string(raw))
	}
	if err := wsjson.Write(ctx, conn, setup); err != nil {
		conn.Close(websocket.StatusInternalError, "")
		return fmt.Errorf("gemini setup: %w", err)
	}

	if err := c.waitForSetupComplete(ctx); err != nil {
		conn.Close(websocket.StatusInternalError, "")
		return fmt.Errorf("gemini setupComplete: %w", err)
	}

	go c.recvLoop(context.Background())
	return nil
}

func (c *geminiClient) waitForSetupComplete(ctx context.Context) error {
	for {
		var msg map[string]json.RawMessage
		if err := wsjson.Read(ctx, c.conn, &msg); err != nil {
			return err
		}
		if _, ok := msg["setupComplete"]; ok {
			return nil
		}
	}
}

func (c *geminiClient) recvLoop(ctx context.Context) {
	defer close(c.done)
	for {
		var msg map[string]json.RawMessage
		if err := wsjson.Read(ctx, c.conn, &msg); err != nil {
			if ctx.Err() == nil {
				c.errCh <- err
			}
			return
		}

		if _, ok := msg["goAway"]; ok {
			c.logger.Info("gemini goAway received - session expiring")
			c.errCh <- fmt.Errorf("gemini goAway - session expiring")
			return
		}

		serverContent, ok := msg["serverContent"]
		if !ok {
			continue
		}

		var sc struct {
			ModelTurn struct {
				Parts []struct {
					InlineData *struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
				} `json:"parts"`
			} `json:"modelTurn"`
		}
		if err := json.Unmarshal(serverContent, &sc); err != nil {
			continue
		}

		for _, part := range sc.ModelTurn.Parts {
			if part.InlineData == nil {
				continue
			}
			if part.InlineData.MimeType != "audio/pcm;rate=24000" {
				continue
			}
			rawBytes, err := base64.StdEncoding.DecodeString(part.InlineData.Data)
			if err != nil {
				c.logger.Debug("gemini: base64 decode error", "err", err)
				continue
			}
			src24, err := audio.BytesToInt16(rawBytes)
			if err != nil {
				continue
			}
			dst8 := make([]int16, len(src24)/3)
			if err := audio.Resample24to8(dst8, src24); err != nil {
				continue
			}
			pcm8 := audio.Int16ToBytes(dst8)
			out := make([]byte, len(pcm8)/2)
			if _, err := audio.EncodeUlaw(out, pcm8); err != nil {
				continue
			}
			if c.logMedia {
				c.logger.Log(ctx, LevelTrace, "gemini rx audio chunk",
					"mime", part.InlineData.MimeType,
					"raw_bytes", len(rawBytes),
					"g711_bytes", len(out),
				)
			}

			c.mu.Lock()
			c.lastRecv = time.Now()
			c.mu.Unlock()

			select {
			case c.recvCh <- out:
			default:
			}
		}
	}
}

func (c *geminiClient) SendAudio(frame []byte) error {
	pcmBuf := make([]byte, audio.FrameBytesPCM)
	if _, err := audio.DecodeUlaw(pcmBuf, frame); err != nil {
		return fmt.Errorf("gemini SendAudio decode: %w", err)
	}

	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	for i := range c.pcm8buf {
		c.pcm8buf[i] = int16(uint16(pcmBuf[2*i]) | uint16(pcmBuf[2*i+1])<<8)
	}
	if err := audio.Resample8to16(c.pcm16buf[:], c.pcm8buf[:]); err != nil {
		return fmt.Errorf("gemini SendAudio resample: %w", err)
	}
	rawBytes := audio.Int16ToBytes(c.pcm16buf[:])
	if c.logMedia {
		c.logger.Log(context.Background(), LevelTrace, "gemini tx audio frame",
			"g711_bytes", len(frame),
			"pcm16_bytes", len(rawBytes),
		)
	}
	encoded := base64.StdEncoding.EncodeToString(rawBytes)

	msg := map[string]any{
		"realtimeInput": map[string]any{
			"mediaChunks": []map[string]any{
				{
					"mimeType": "audio/pcm;rate=16000",
					"data":     encoded,
				},
			},
		},
	}
	return wsjson.Write(context.Background(), c.conn, msg)
}

func (c *geminiClient) RecvAudio(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case err := <-c.errCh:
		return nil, err
	case <-c.done:
		return nil, io.EOF
	case pcm := <-c.recvCh:
		return pcm, nil
	}
}

func (c *geminiClient) Ping(ctx context.Context) error {
	c.mu.Lock()
	last := c.lastRecv
	c.mu.Unlock()
	if !last.IsZero() && time.Since(last) > 30*time.Second {
		return fmt.Errorf("gemini: no data for >30s")
	}
	select {
	case <-c.done:
		return fmt.Errorf("gemini: receive loop terminated")
	default:
	}
	pingMsg := map[string]any{
		"clientContent": map[string]any{
			"turns":        []any{},
			"turnComplete": false,
		},
	}
	c.logger.Log(context.Background(), LevelTrace, "gemini tx ping")
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return wsjson.Write(context.Background(), c.conn, pingMsg)
}

func (c *geminiClient) Close() error {
	if c.conn != nil {
		c.logger.Info("gemini: closing session")
		return c.conn.Close(websocket.StatusNormalClosure, "")
	}
	return nil
}
