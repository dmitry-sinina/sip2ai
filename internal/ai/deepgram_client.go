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

	agentws "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/agent/v1/websocket"
	msgifaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/agent/v1/websocket/interfaces"
	clientv1 "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces/v1"
	"sip2ai/internal/config"
	"sip2ai/internal/metrics"
)

// deepgramHandler is a minimal AgentMessageChan implementation that owns only
// the channels we need. Everything else returns an empty slice so the SDK
// router skips those events without blocking.
type deepgramHandler struct {
	binaryCh          chan *[]byte
	welcomeCh         chan *msgifaces.WelcomeResponse
	settingsAppliedCh chan *msgifaces.SettingsAppliedResponse
	functionCallCh    chan *msgifaces.FunctionCallRequestResponse
	errorCh           chan *msgifaces.ErrorResponse
	closeCh           chan *msgifaces.CloseResponse
}

func newDeepgramHandler() *deepgramHandler {
	return &deepgramHandler{
		binaryCh:          make(chan *[]byte, 64),
		welcomeCh:         make(chan *msgifaces.WelcomeResponse, 1),
		settingsAppliedCh: make(chan *msgifaces.SettingsAppliedResponse, 1),
		functionCallCh:    make(chan *msgifaces.FunctionCallRequestResponse, 4),
		errorCh:           make(chan *msgifaces.ErrorResponse, 4),
		closeCh:           make(chan *msgifaces.CloseResponse, 1),
	}
}

func (h *deepgramHandler) GetBinary() []*chan *[]byte {
	return []*chan *[]byte{&h.binaryCh}
}
func (h *deepgramHandler) GetWelcome() []*chan *msgifaces.WelcomeResponse {
	return []*chan *msgifaces.WelcomeResponse{&h.welcomeCh}
}
func (h *deepgramHandler) GetSettingsApplied() []*chan *msgifaces.SettingsAppliedResponse {
	return []*chan *msgifaces.SettingsAppliedResponse{&h.settingsAppliedCh}
}
func (h *deepgramHandler) GetFunctionCallRequest() []*chan *msgifaces.FunctionCallRequestResponse {
	return []*chan *msgifaces.FunctionCallRequestResponse{&h.functionCallCh}
}
func (h *deepgramHandler) GetError() []*chan *msgifaces.ErrorResponse {
	return []*chan *msgifaces.ErrorResponse{&h.errorCh}
}
func (h *deepgramHandler) GetClose() []*chan *msgifaces.CloseResponse {
	return []*chan *msgifaces.CloseResponse{&h.closeCh}
}

// Unused — return empty so router skips them.
func (h *deepgramHandler) GetOpen() []*chan *msgifaces.OpenResponse                           { return nil }
func (h *deepgramHandler) GetConversationText() []*chan *msgifaces.ConversationTextResponse   { return nil }
func (h *deepgramHandler) GetUserStartedSpeaking() []*chan *msgifaces.UserStartedSpeakingResponse { return nil }
func (h *deepgramHandler) GetAgentThinking() []*chan *msgifaces.AgentThinkingResponse         { return nil }
func (h *deepgramHandler) GetAgentStartedSpeaking() []*chan *msgifaces.AgentStartedSpeakingResponse { return nil }
func (h *deepgramHandler) GetAgentAudioDone() []*chan *msgifaces.AgentAudioDoneResponse       { return nil }
func (h *deepgramHandler) GetInjectionRefused() []*chan *msgifaces.InjectionRefusedResponse   { return nil }
func (h *deepgramHandler) GetKeepAlive() []*chan *msgifaces.KeepAlive                         { return nil }
func (h *deepgramHandler) GetUnhandled() []*chan *[]byte                                       { return nil }

// ─────────────────────────────────────────────────────────────────────────────

type deepgramClient struct {
	cfg      *config.DeepgramConfig
	logger   *slog.Logger
	logMedia bool
	metrics  *metrics.Recorder
	ws       *agentws.WSChannel
	handler  *deepgramHandler

	wsCancel   context.CancelFunc // cancels the WSChannel context on Close()
	codec      string             // SIP codec negotiated for this session (e.g. "PCMU")
	transferCh chan TransferRequest

	recvCh chan []byte
	errCh  chan error
	done   chan struct{}

	mu       sync.Mutex
	lastRecv time.Time
}

func newDeepgramClient(cfg *config.DeepgramConfig, logger *slog.Logger, logMedia bool, rec *metrics.Recorder) *deepgramClient {
	return &deepgramClient{
		cfg:        cfg,
		logger:     logger,
		logMedia:   logMedia,
		metrics:    rec,
		transferCh: make(chan TransferRequest, 1),
		recvCh:     make(chan []byte, 64),
		errCh:      make(chan error, 4),
		done:       make(chan struct{}),
	}
}

func (c *deepgramClient) TransferCh() <-chan TransferRequest {
	return c.transferCh
}

func (c *deepgramClient) Connect(ctx context.Context, sipCodec string, sipRate uint32) error {
	_ = sipRate // deepgram derives codec params from sipCodec only
	c.codec = sipCodec
	enc, rate := codecToDeepgram(sipCodec)
	settings := clientv1.NewSettingsOptions()
	settings.Audio.Input = &clientv1.Input{
		Encoding:   enc,
		SampleRate: rate,
	}
	settings.Audio.Output = &clientv1.Output{
		Encoding:   enc,
		SampleRate: rate,
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
	if c.cfg.HangupToolDesc != "" {
		settings.Agent.Think.Functions = &[]clientv1.Functions{
			{
				Name:        "hangup_call",
				Description: c.cfg.HangupToolDesc,
				Parameters:  clientv1.Parameters{Type: "object"},
			},
		}
	}

	if raw, err := json.Marshal(settings); err == nil {
		c.logger.Log(ctx, LevelTrace, "deepgram tx connect settings", "settings", string(raw))
	}

	c.handler = newDeepgramHandler()

	cOpts := &clientv1.ClientOptions{}
	if c.cfg.Proxy != "" {
		proxyURL, err := url.Parse(c.cfg.Proxy)
		if err != nil {
			return fmt.Errorf("deepgram proxy URL: %w", err)
		}
		cOpts.Proxy = http.ProxyURL(proxyURL)
	}

	// Use an independent context for the WSChannel so that cancelling the
	// connect-timeout context (in ConnectWithRetry) does not kill the live
	// WebSocket goroutines. The connection is torn down explicitly via Close().
	if c.wsCancel != nil {
		c.wsCancel()
	}
	wsCtx, wsCancel := context.WithCancel(context.Background())
	c.wsCancel = wsCancel

	ws, err := agentws.NewUsingChan(wsCtx, c.cfg.APIKey, cOpts, settings, c.handler)
	if err != nil {
		return fmt.Errorf("deepgram new client: %w", err)
	}
	c.ws = ws

	if ok := ws.Connect(); !ok {
		return fmt.Errorf("deepgram: Connect() returned false")
	}
	// SDK calls Start() internally on Connect(), which sends Settings immediately
	// before Welcome arrives — Deepgram ignores that early send. We wait for
	// Welcome and then send Settings ourselves.

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.handler.welcomeCh:
		c.logger.Debug("deepgram: welcome received")
	case <-time.After(10 * time.Second):
		return fmt.Errorf("deepgram: timeout waiting for Welcome")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.handler.settingsAppliedCh:
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

	for {
		select {
		case audioPtr, ok := <-c.handler.binaryCh:
			if !ok {
				return
			}
			if audioPtr == nil {
				continue
			}
			c.mu.Lock()
			c.lastRecv = time.Now()
			c.mu.Unlock()

			out := *audioPtr
			if c.logMedia {
				c.logger.Log(context.Background(), LevelTrace, "deepgram rx audio frame", "codec", c.codec, "bytes", len(out))
			}
			select {
			case c.recvCh <- out:
			default:
			}

		case req, ok := <-c.handler.functionCallCh:
			if !ok {
				return
			}
			if req != nil {
				c.handleFunctionCall(req)
			}

		case errPtr, ok := <-c.handler.errorCh:
			if !ok {
				return
			}
			if errPtr != nil {
				c.metrics.AIError("deepgram", "stream_error")
				c.errCh <- fmt.Errorf("deepgram error: %+v", errPtr)
			}
			return

		case _, ok := <-c.handler.closeCh:
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
		c.logger.Log(context.Background(), LevelTrace, "deepgram tx audio frame", "codec", c.codec, "bytes", len(frame))
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

func (c *deepgramClient) handleFunctionCall(req *msgifaces.FunctionCallRequestResponse) {
	c.logger.Info("deepgram: function call requested", "function", req.FunctionName, "call_id", req.FunctionCallID)

	resp := msgifaces.FunctionCallResponse{
		Type:           string(msgifaces.TypeFunctionCallResponse),
		FunctionCallID: req.FunctionCallID,
		Output:         `{"status":"ok"}`,
	}
	if err := c.ws.WriteJSON(resp); err != nil {
		c.logger.Warn("deepgram: failed to send function call response", "err", err)
	}

	switch req.FunctionName {
	case "hangup_call":
		c.logger.Info("deepgram: hangup requested by AI")
		select {
		case c.transferCh <- TransferRequest{}: // empty destination = hangup
		default:
		}
	default:
		c.logger.Warn("deepgram: unknown function call", "function", req.FunctionName)
	}
}

func (c *deepgramClient) Close() error {
	if c.ws != nil {
		c.logger.Info("deepgram: closing session")
		c.ws.Stop()
	}
	if c.wsCancel != nil {
		c.wsCancel()
	}
	return nil
}

func codecToDeepgram(codec string) (encoding string, sampleRate int) {
	switch codec {
	case "PCMA":
		return "alaw", 8000
	case "L16":
		return "linear16", 24000
	default:
		return "mulaw", 8000
	}
}
