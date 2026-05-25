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

	"sip2ai/internal/config"
	"sip2ai/internal/metrics"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

const openAIEndpoint = "wss://api.openai.com/v1/realtime"

type openAIUsage struct {
	TotalTokens  int
	InputTokens  int
	OutputTokens int
	InputText    int
	InputAudio   int
	OutputText   int
	OutputAudio  int
}

type openAIClient struct {
	cfg        *config.OpenAIConfig
	transfers  map[string]string
	logger     *slog.Logger
	logMedia   bool
	metrics    *metrics.Recorder
	conn       *websocket.Conn
	recvCh     chan []byte
	errCh      chan error
	transferCh chan TransferRequest
	sendMu     sync.Mutex
	done       chan struct{}
	lastRecv   time.Time
	mu         sync.Mutex
	usage      openAIUsage

	// Barge-in / interrupt handling.
	interruptHandler   func() int // installed by CallSession; flushes adapter, returns pending bytes
	outputBytesPerMs   int        // derived from session output audio format
	currentItemID      string     // item_id of the audio response currently being streamed
	currentItemBytes   int        // total decoded audio bytes queued for currentItemID
	responseInProgress bool       // true between response.created and response.done
}

func newOpenAIClient(cfg *config.OpenAIConfig, transfers map[string]string, logger *slog.Logger, logMedia bool, rec *metrics.Recorder) *openAIClient {
	return &openAIClient{
		cfg:        cfg,
		transfers:  transfers,
		logger:     logger,
		logMedia:   logMedia,
		metrics:    rec,
		recvCh:     make(chan []byte, 256),
		errCh:      make(chan error, 4),
		transferCh: make(chan TransferRequest, 1),
		done:       make(chan struct{}),
	}
}

func (c *openAIClient) TransferCh() <-chan TransferRequest {
	return c.transferCh
}

// SetInterruptHandler installs the barge-in callback. Must be called before
// the recv loop sees any speech_started events (i.e. before audio flows).
func (c *openAIClient) SetInterruptHandler(h func() int) {
	c.interruptHandler = h
}

func (c *openAIClient) Connect(ctx context.Context, sipCodec string, sipRate uint32) error {
	wsURL := fmt.Sprintf("%s?model=%s", openAIEndpoint, c.cfg.Model)
	dialOpts := &websocket.DialOptions{
		HTTPHeader: map[string][]string{
			"Authorization": {"Bearer " + c.cfg.APIKey},
		},
	}
	if c.cfg.Proxy != "" {
		proxyURL, err := url.Parse(c.cfg.Proxy)
		if err != nil {
			return fmt.Errorf("openai proxy URL: %w", err)
		}
		dialOpts.HTTPClient = &http.Client{
			Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)},
		}
	}
	conn, _, err := websocket.Dial(ctx, wsURL, dialOpts)
	if err != nil {
		return fmt.Errorf("openai dial: %w", err)
	}
	conn.SetReadLimit(-1)
	c.conn = conn

	if err := c.waitForEvent(ctx, "session.created"); err != nil {
		conn.Close(websocket.StatusInternalError, "")
		return fmt.Errorf("openai session.created: %w", err)
	}

	audioFmt := codecToOpenAI(sipCodec, sipRate)
	c.outputBytesPerMs = outputBytesPerMs(audioFmt)
	session := map[string]any{
		"type":              "realtime",
		"instructions":      c.cfg.SystemPrompt,
		"output_modalities": []string{"audio"},
		"audio": map[string]any{
			"input": map[string]any{
				"format": audioFmt,
				"turn_detection": map[string]any{
					"type":                "server_vad",
					"silence_duration_ms": 500,
					"create_response":     true,
				},
			},
			"output": map[string]any{
				"format": audioFmt,
				"voice":  c.cfg.Voice,
			},
		},
	}
	tools := []map[string]any{
		{
			"type":        "function",
			"name":        "hangup_call",
			"description": c.cfg.HangupToolDesc,
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
	if len(c.transfers) > 0 {
		destinations := make([]string, 0, len(c.transfers))
		for name := range c.transfers {
			destinations = append(destinations, name)
		}
		tools = append(tools, map[string]any{
			"type":        "function",
			"name":        "transfer_call",
			"description": c.cfg.TransferToolDesc,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"destination": map[string]any{
						"type":        "string",
						"enum":        destinations,
						"description": "The department or person to transfer to",
					},
				},
				"required": []string{"destination"},
			},
		})
	}
	session["tools"] = tools
	update := map[string]any{
		"type":    "session.update",
		"session": session,
	}
	if raw, err := json.MarshalIndent(update, "", "  "); err == nil {
		c.logger.Log(ctx, LevelTrace, "openai tx session.update\n"+string(raw))
	}
	if err := wsjson.Write(ctx, conn, update); err != nil {
		conn.Close(websocket.StatusInternalError, "")
		return fmt.Errorf("openai session.update: %w", err)
	}

	go c.recvLoop(context.Background())

	if c.cfg.Greeting != "" {
		greet := map[string]any{
			"type": "response.create",
			"response": map[string]any{
				"instructions": "Say the following greeting to the caller, then wait for their response: " + c.cfg.Greeting,
			},
		}
		c.sendMu.Lock()
		err := wsjson.Write(ctx, c.conn, greet)
		c.sendMu.Unlock()
		if err != nil {
			conn.Close(websocket.StatusInternalError, "")
			return fmt.Errorf("openai greeting: %w", err)
		}
		c.logger.Debug("openai: greeting triggered", "greeting", c.cfg.Greeting)
	}

	return nil
}

func (c *openAIClient) waitForEvent(ctx context.Context, eventType string) error {
	for {
		var msg map[string]json.RawMessage
		if err := wsjson.Read(ctx, c.conn, &msg); err != nil {
			return err
		}
		var t string
		json.Unmarshal(msg["type"], &t) //nolint:errcheck
		if raw, err := json.MarshalIndent(msg, "", "  "); err == nil {
			c.logger.Log(ctx, LevelTrace, fmt.Sprintf("openai rx pre-setup event type=%s\n%s", t, raw))
		}
		if t == eventType {
			return nil
		}
	}
}

func (c *openAIClient) recvLoop(ctx context.Context) {
	defer close(c.done)
	for {
		var msg map[string]json.RawMessage
		if err := wsjson.Read(ctx, c.conn, &msg); err != nil {
			if ctx.Err() == nil {
				c.errCh <- err
			}
			return
		}
		c.mu.Lock()
		c.lastRecv = time.Now()
		c.mu.Unlock()

		typeRaw, ok := msg["type"]
		if !ok {
			continue
		}
		var eventType string
		json.Unmarshal(typeRaw, &eventType) //nolint:errcheck

		switch eventType {
		case "response.output_audio.delta", "response.audio.delta":
			audioRaw, ok := msg["delta"]
			if !ok {
				continue
			}
			var encoded string
			if err := json.Unmarshal(audioRaw, &encoded); err != nil {
				continue
			}
			// output_audio_format is g711_ulaw; pass through directly to RTP.
			g711Data, err := base64.StdEncoding.DecodeString(encoded)
			if err != nil {
				c.logger.Debug("openai: base64 decode error", "err", err)
				continue
			}
			// Track per-item byte count so a later barge-in can tell OpenAI
			// exactly how many ms the caller actually heard.
			if itemRaw, ok := msg["item_id"]; ok {
				var itemID string
				if err := json.Unmarshal(itemRaw, &itemID); err == nil && itemID != "" {
					if itemID != c.currentItemID {
						c.currentItemID = itemID
						c.currentItemBytes = 0
					}
					c.currentItemBytes += len(g711Data)
				}
			}
			if c.logMedia {
				c.logger.Log(ctx, LevelTrace, "openai rx audio delta", "g711_bytes", len(g711Data))
			}
			select {
			case c.recvCh <- g711Data:
			default:
				c.logger.Warn("openai: recvCh full, dropping audio frame", "g711_bytes", len(g711Data))
			}
		case "input_audio_buffer.speech_started":
			c.handleBargeIn(ctx)
			if raw, err := json.MarshalIndent(msg, "", "  "); err == nil {
				c.logger.Log(ctx, LevelTrace, fmt.Sprintf("openai rx event type=%s\n%s", eventType, raw))
			}
		case "response.created":
			c.responseInProgress = true
			if raw, err := json.MarshalIndent(msg, "", "  "); err == nil {
				c.logger.Log(ctx, LevelTrace, fmt.Sprintf("openai rx event type=%s\n%s", eventType, raw))
			}
		case "response.output_item.done":
			c.handleOutputItem(ctx, msg)
			if raw, err := json.MarshalIndent(msg, "", "  "); err == nil {
				c.logger.Log(ctx, LevelTrace, fmt.Sprintf("openai rx event type=%s\n%s", eventType, raw))
			}
		case "response.done":
			c.parseUsage(msg)
			c.responseInProgress = false
			// Item finished cleanly — no truncation will apply to it anymore.
			c.currentItemID = ""
			c.currentItemBytes = 0
			if raw, err := json.MarshalIndent(msg, "", "  "); err == nil {
				c.logger.Log(ctx, LevelTrace, fmt.Sprintf("openai rx event type=%s\n%s", eventType, raw))
			}
		case "error":
			// Application-level errors (e.g. "cancel failed: no active
			// response") are per-request and must not tear down the call.
			// Real connection failures surface via the wsjson.Read error
			// path above instead.
			c.metrics.AIError("openai", "server_error")
			errRaw, _ := json.Marshal(msg)
			c.logger.Warn("openai: server error event", "raw", string(errRaw))
		default:
			if raw, err := json.MarshalIndent(msg, "", "  "); err == nil {
				c.logger.Log(ctx, LevelTrace, fmt.Sprintf("openai rx event type=%s\n%s", eventType, raw))
			}
		}
	}
}

func (c *openAIClient) SendAudio(frame []byte) error {
	if c.logMedia {
		c.logger.Log(context.Background(), LevelTrace, "openai tx audio frame", "g711_bytes", len(frame))
	}
	encoded := base64.StdEncoding.EncodeToString(frame)
	msg := map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": encoded,
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	return wsjson.Write(context.Background(), c.conn, msg)
}

func (c *openAIClient) RecvAudio(ctx context.Context) ([]byte, error) {
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

func (c *openAIClient) Ping(ctx context.Context) error {
	c.mu.Lock()
	last := c.lastRecv
	c.mu.Unlock()
	if !last.IsZero() && time.Since(last) > 30*time.Second {
		return fmt.Errorf("openai: no data for >30s")
	}
	select {
	case <-c.done:
		return fmt.Errorf("openai: receive loop terminated")
	default:
	}
	return nil
}

func (c *openAIClient) Close() error {
	if c.conn == nil {
		return nil
	}

	c.mu.Lock()
	u := c.usage
	c.mu.Unlock()

	c.logger.Info("openai: session closed",
		"total_tokens", u.TotalTokens,
		"input_tokens", u.InputTokens,
		"output_tokens", u.OutputTokens,
		"input_text_tokens", u.InputText,
		"input_audio_tokens", u.InputAudio,
		"output_text_tokens", u.OutputText,
		"output_audio_tokens", u.OutputAudio,
	)
	return c.conn.Close(websocket.StatusNormalClosure, "")
}

func (c *openAIClient) handleOutputItem(ctx context.Context, msg map[string]json.RawMessage) {
	itemRaw, ok := msg["item"]
	if !ok {
		return
	}
	var item struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
		Args   string `json:"arguments"`
	}
	if err := json.Unmarshal(itemRaw, &item); err != nil || item.Type != "function_call" {
		return
	}

	switch item.Name {
	case "hangup_call":
		c.logger.Info("openai: hangup requested")
		c.sendFunctionResult(ctx, item.CallID, `{"status": "hanging_up"}`)
		c.sendResponseCreate(ctx, "Say goodbye to the caller briefly.")
		select {
		case c.transferCh <- TransferRequest{}: // empty destination = hangup
		default:
		}

	case "transfer_call":
		var args struct {
			Destination string `json:"destination"`
		}
		if err := json.Unmarshal([]byte(item.Args), &args); err != nil {
			c.logger.Error("openai: failed to parse transfer args", "err", err, "args", item.Args)
			return
		}
		phoneNumber, ok := c.transfers[args.Destination]
		if !ok {
			c.logger.Warn("openai: unknown transfer destination", "destination", args.Destination)
			c.sendFunctionResult(ctx, item.CallID, `{"error": "unknown destination"}`)
			return
		}
		c.logger.Info("openai: transfer requested", "destination", args.Destination, "number", phoneNumber)
		c.sendFunctionResult(ctx, item.CallID, `{"status": "transferring"}`)
		c.sendResponseCreate(ctx, "Tell the caller you are transferring them now. Keep it brief.")
		select {
		case c.transferCh <- TransferRequest{Destination: phoneNumber}:
		default:
		}
	}
}

func (c *openAIClient) sendResponseCreate(ctx context.Context, instructions string) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	wsjson.Write(ctx, c.conn, map[string]any{ //nolint:errcheck
		"type": "response.create",
		"response": map[string]any{
			"instructions": instructions,
		},
	})
}

func (c *openAIClient) sendFunctionResult(ctx context.Context, callID, output string) {
	msg := map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  output,
		},
	}
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	wsjson.Write(ctx, c.conn, msg) //nolint:errcheck
}

func (c *openAIClient) parseUsage(msg map[string]json.RawMessage) {
	respRaw, ok := msg["response"]
	if !ok {
		return
	}
	var resp struct {
		Usage struct {
			TotalTokens  int `json:"total_tokens"`
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			InputDetail  struct {
				TextTokens  int `json:"text_tokens"`
				AudioTokens int `json:"audio_tokens"`
			} `json:"input_token_details"`
			OutputDetail struct {
				TextTokens  int `json:"text_tokens"`
				AudioTokens int `json:"audio_tokens"`
			} `json:"output_token_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respRaw, &resp); err != nil {
		return
	}
	u := resp.Usage
	c.mu.Lock()
	c.usage.TotalTokens += u.TotalTokens
	c.usage.InputTokens += u.InputTokens
	c.usage.OutputTokens += u.OutputTokens
	c.usage.InputText += u.InputDetail.TextTokens
	c.usage.InputAudio += u.InputDetail.AudioTokens
	c.usage.OutputText += u.OutputDetail.TextTokens
	c.usage.OutputAudio += u.OutputDetail.AudioTokens
	c.mu.Unlock()
	c.metrics.TokensUsed("openai", u.InputDetail.TextTokens, u.InputDetail.AudioTokens, u.OutputDetail.TextTokens, u.OutputDetail.AudioTokens)
}

// handleBargeIn is called when OpenAI VAD reports caller speech started while
// we may still be playing AI audio. We must (a) stop sending more audio to
// the caller and (b) tell OpenAI how much of the current item the caller
// actually heard, so its conversation history is truthful.
func (c *openAIClient) handleBargeIn(ctx context.Context) {
	if c.interruptHandler == nil {
		return
	}
	// Drain anything already queued in recvCh — those bytes were counted in
	// currentItemBytes but haven't even reached the adapter yet, so from the
	// caller's perspective they were never heard.
	drainedRecvBytes := 0
DRAIN:
	for {
		select {
		case d := <-c.recvCh:
			drainedRecvBytes += len(d)
		default:
			break DRAIN
		}
	}
	// Flush the downstream playback buffer; result is bytes that had arrived
	// at the adapter but were not yet shipped on RTP.
	pendingAdapter := c.interruptHandler()

	itemID := c.currentItemID
	playedBytes := c.currentItemBytes - drainedRecvBytes - pendingAdapter
	if playedBytes < 0 {
		playedBytes = 0
	}
	playedMs := 0
	if c.outputBytesPerMs > 0 {
		playedMs = playedBytes / c.outputBytesPerMs
	}

	if itemID != "" && playedMs > 0 {
		truncate := map[string]any{
			"type":          "conversation.item.truncate",
			"item_id":       itemID,
			"content_index": 0,
			"audio_end_ms":  playedMs,
		}
		c.sendMu.Lock()
		err := wsjson.Write(ctx, c.conn, truncate)
		c.sendMu.Unlock()
		if err != nil {
			c.logger.Warn("openai: truncate write failed", "err", err)
		}
	}

	if c.responseInProgress {
		c.sendMu.Lock()
		err := wsjson.Write(ctx, c.conn, map[string]any{"type": "response.cancel"})
		c.sendMu.Unlock()
		if err != nil {
			c.logger.Warn("openai: response.cancel write failed", "err", err)
		}
	}

	c.logger.Info("openai: barge-in",
		"item_id", itemID,
		"played_ms", playedMs,
		"dropped_adapter_bytes", pendingAdapter,
		"dropped_recvch_bytes", drainedRecvBytes,
		"response_in_progress", c.responseInProgress,
	)

	c.currentItemID = ""
	c.currentItemBytes = 0
}

// outputBytesPerMs returns the byte rate per millisecond implied by the
// OpenAI output audio format object. G.711 is 8 kHz × 1 byte = 8 bytes/ms;
// PCM at 24 kHz × 2 bytes = 48 bytes/ms.
func outputBytesPerMs(format map[string]any) int {
	t, _ := format["type"].(string)
	switch t {
	case "audio/pcmu", "audio/pcma":
		return 8
	case "audio/pcm":
		rate := 24000
		switch r := format["rate"].(type) {
		case int:
			if r > 0 {
				rate = r
			}
		case float64:
			if r > 0 {
				rate = int(r)
			}
		}
		return (rate * 2) / 1000
	}
	return 8
}

func codecToOpenAI(codec string, rate uint32) map[string]any {
	switch codec {
	case "PCMA":
		return map[string]any{"type": "audio/pcma"}
	case "L16":
		r := 24000
		if rate > 0 {
			r = int(rate)
		}
		return map[string]any{"type": "audio/pcm", "rate": r}
	default:
		return map[string]any{"type": "audio/pcmu"}
	}
}
