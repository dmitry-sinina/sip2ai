package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	agentchannel "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/agent/v1/websocket"
	agentws "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/agent/v1/websocket"
	clientv1 "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces/v1"
	"sip2ai/internal/config"
)

type deepgramClient struct {
	cfg      *config.DeepgramConfig
	logger   *slog.Logger
	logMedia bool
	ws       *agentws.WSChannel
	handler *agentchannel.DefaultChanHandler

	recvCh chan []byte
	errCh  chan error
	done   chan struct{}

	mu       sync.Mutex
	lastRecv time.Time
}

func newDeepgramClient(cfg *config.DeepgramConfig, logger *slog.Logger, logMedia bool) *deepgramClient {
	return &deepgramClient{
		cfg:      cfg,
		logger:   logger,
		logMedia: logMedia,
		recvCh:   make(chan []byte, 64),
		errCh:    make(chan error, 4),
		done:     make(chan struct{}),
	}
}

func (c *deepgramClient) Connect(ctx context.Context) error {
	settings := clientv1.NewSettingsOptions()
	settings.Audio.Input = &clientv1.Input{
		Encoding:   "mulaw",
		SampleRate: 8000,
	}
	settings.Audio.Output = &clientv1.Output{
		Encoding:   "mulaw",
		SampleRate: 8000,
		Container:  "none",
	}
	settings.Agent.Listen.Provider = map[string]interface{}{
		"type":  "deepgram",
		"model": c.cfg.ListenModel,
	}
	settings.Agent.Think.Provider = map[string]interface{}{
		"type":  "open_ai",
		"model": c.cfg.ThinkModel,
	}
	settings.Agent.Speak.Provider = map[string]interface{}{
		"type":  "deepgram",
		"model": c.cfg.SpeakModel,
	}
	settings.Agent.Greeting = c.cfg.Greeting

	if raw, err := json.Marshal(settings); err == nil {
		c.logger.Log(ctx, LevelTrace, "deepgram tx connect settings", "settings", string(raw))
	}
	c.handler = agentchannel.NewDefaultChanHandler()

	cOpts := &clientv1.ClientOptions{}
	if c.cfg.Proxy != "" {
		proxyURL, err := url.Parse(c.cfg.Proxy)
		if err != nil {
			return fmt.Errorf("deepgram proxy URL: %w", err)
		}
		cOpts.Proxy = http.ProxyURL(proxyURL)
	}
	ws, err := agentws.NewUsingChan(ctx, c.cfg.APIKey, cOpts, settings, c.handler)
	if err != nil {
		return fmt.Errorf("deepgram new client: %w", err)
	}
	c.ws = ws

	if ok := ws.Connect(); !ok {
		return fmt.Errorf("deepgram: Connect() returned false")
	}
	ws.Start()

	settingsAppliedPtrs := c.handler.GetSettingsApplied()
	if len(settingsAppliedPtrs) == 0 {
		return fmt.Errorf("deepgram: no SettingsApplied channel available")
	}
	settingsAppliedCh := *settingsAppliedPtrs[0]
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-settingsAppliedCh:
		c.logger.Debug("deepgram: settings applied")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("deepgram: timeout waiting for SettingsApplied")
	}

	c.done = make(chan struct{})
	go c.recvLoop()
	return nil
}

func (c *deepgramClient) recvLoop() {
	defer close(c.done)

	binaryPtrs := c.handler.GetBinary()
	errorPtrs := c.handler.GetError()
	closePtrs := c.handler.GetClose()

	if len(binaryPtrs) == 0 || len(errorPtrs) == 0 || len(closePtrs) == 0 {
		c.errCh <- fmt.Errorf("deepgram: handler channels not initialised")
		return
	}

	binaryCh := *binaryPtrs[0]
	errorCh := *errorPtrs[0]
	closeCh := *closePtrs[0]

	for {
		select {
		case audioPtr, ok := <-binaryCh:
			if !ok {
				return
			}
			if audioPtr == nil {
				continue
			}
			c.mu.Lock()
			c.lastRecv = time.Now()
			c.mu.Unlock()

			// Deepgram outputs g711 ulaw natively; pass through directly to RTP.
			out := *audioPtr
			if c.logMedia {
					c.logger.Log(context.Background(), LevelTrace, "deepgram rx audio frame", "g711_bytes", len(out))
				}
			select {
			case c.recvCh <- out:
			default:
			}

		case errPtr, ok := <-errorCh:
			if !ok {
				return
			}
			if errPtr != nil {
				c.errCh <- fmt.Errorf("deepgram error: %+v", errPtr)
			}
			return

		case _, ok := <-closeCh:
			if !ok {
				return
			}
			c.logger.Debug("deepgram: close received")
			return
		}
	}
}

func (c *deepgramClient) SendAudio(frame []byte) error {
	if c.logMedia {
		c.logger.Log(context.Background(), LevelTrace, "deepgram tx audio frame", "g711_bytes", len(frame))
	}
	return c.ws.WriteBinary(frame)
}

func (c *deepgramClient) RecvAudio(ctx context.Context) ([]byte, error) {
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

func (c *deepgramClient) Ping(ctx context.Context) error {
	c.mu.Lock()
	last := c.lastRecv
	c.mu.Unlock()
	if !last.IsZero() && time.Since(last) > 30*time.Second {
		return fmt.Errorf("deepgram: no data for >30s")
	}
	select {
	case <-c.done:
		return fmt.Errorf("deepgram: receive loop terminated")
	default:
	}
	return c.ws.KeepAlive()
}

func (c *deepgramClient) Close() error {
	if c.ws != nil {
		c.logger.Info("deepgram: closing session")
		c.ws.Stop()
	}
	return nil
}
