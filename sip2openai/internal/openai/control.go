package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

// keepaliveInterval pings the sideband WS to defeat OpenAI's idle-drop on
// silent calls. staleTimeout declares the control plane dead if no event
// arrives for this long.
const (
	keepaliveInterval = 20 * time.Second
	staleTimeout      = 45 * time.Second
)

// ControlOptions configures the Realtime session over the sideband WebSocket.
// Audio formats are intentionally omitted: in WebRTC mode media is negotiated
// via SDP and flows on the RTP track, never over this socket.
type ControlOptions struct {
	Voice        string
	Instructions string
	Greeting     string
	HangupDesc   string
	TransferDesc string
	Transfers    map[string]string // destination name -> SIP/tel URI (used by M3 REFER)
}

// Control is the per-call sideband connection to OpenAI: it configures the
// session, triggers the greeting, dispatches tool calls, and tallies tokens.
type Control struct {
	apiKey string
	wsURL  string
	opts   ControlOptions
	log    *slog.Logger

	// OnHangup is invoked when the model calls hangup_call.
	OnHangup func()
	// OnTransfer is invoked when the model calls transfer_call with a resolved
	// destination URI (M3 maps this to a SIP REFER).
	OnTransfer func(destURI string)

	conn   *websocket.Conn
	sendMu sync.Mutex

	mu       sync.Mutex
	lastRecv time.Time
	usage    usage

	done      chan struct{}
	closeOnce sync.Once
}

type usage struct {
	total, input, output int
	inText, inAudio      int
	outText, outAudio    int
}

// NewControl builds a control client for an existing call_id. The WebSocket URL
// is derived from the client's BaseURL (https->wss).
func (c *Client) NewControl(callID string, opts ControlOptions, log *slog.Logger) *Control {
	return &Control{
		apiKey: c.APIKey,
		wsURL:  wsURLFromBase(c.BaseURL, callID),
		opts:   opts,
		log:    log,
		done:   make(chan struct{}),
	}
}

func wsURLFromBase(base, callID string) string {
	switch {
	case strings.HasPrefix(base, "https://"):
		base = "wss://" + strings.TrimPrefix(base, "https://")
	case strings.HasPrefix(base, "http://"):
		base = "ws://" + strings.TrimPrefix(base, "http://")
	}
	return fmt.Sprintf("%s/v1/realtime?call_id=%s", strings.TrimRight(base, "/"), callID)
}

// Start dials the sideband WS, applies session config, fires the greeting, and
// launches the receive + keepalive loops. It returns an error only if the
// initial dial/config fails; the call's media is unaffected either way.
func (ct *Control) Start(ctx context.Context) error {
	conn, _, err := websocket.Dial(ctx, ct.wsURL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Authorization": {"Bearer " + ct.apiKey}},
	})
	if err != nil {
		return fmt.Errorf("dial sideband: %w", err)
	}
	conn.SetReadLimit(-1)
	ct.conn = conn
	ct.mu.Lock()
	ct.lastRecv = time.Now()
	ct.mu.Unlock()

	if err := ct.sendSessionUpdate(ctx); err != nil {
		conn.Close(websocket.StatusInternalError, "")
		return fmt.Errorf("session.update: %w", err)
	}
	if ct.opts.Greeting != "" {
		ct.sendResponseCreate(ctx, "Say the following greeting to the caller, then wait for their response: "+ct.opts.Greeting)
		ct.log.Debug("greeting triggered", "greeting", ct.opts.Greeting)
	}

	go ct.recvLoop()
	go ct.keepalive()
	return nil
}

// Close tears down the sideband WS and logs the call's token usage.
func (ct *Control) Close() error {
	ct.closeOnce.Do(func() { close(ct.done) })
	if ct.conn == nil {
		return nil
	}
	ct.mu.Lock()
	u := ct.usage
	ct.mu.Unlock()
	ct.log.Info("sideband closed",
		"total_tokens", u.total, "input_tokens", u.input, "output_tokens", u.output,
		"input_audio_tokens", u.inAudio, "output_audio_tokens", u.outAudio)
	return ct.conn.Close(websocket.StatusNormalClosure, "")
}

func (ct *Control) sendSessionUpdate(ctx context.Context) error {
	tools := []map[string]any{{
		"type":        "function",
		"name":        "hangup_call",
		"description": ct.opts.HangupDesc,
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	}}
	if len(ct.opts.Transfers) > 0 {
		dests := make([]string, 0, len(ct.opts.Transfers))
		for name := range ct.opts.Transfers {
			dests = append(dests, name)
		}
		tools = append(tools, map[string]any{
			"type":        "function",
			"name":        "transfer_call",
			"description": ct.opts.TransferDesc,
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"destination": map[string]any{
						"type":        "string",
						"enum":        dests,
						"description": "The department or person to transfer to",
					},
				},
				"required": []string{"destination"},
			},
		})
	}

	// No audio.*.format here: WebRTC media is negotiated via SDP.
	session := map[string]any{
		"type":              "realtime",
		"instructions":      ct.opts.Instructions,
		"output_modalities": []string{"audio"},
		"audio": map[string]any{
			"input": map[string]any{
				"turn_detection": map[string]any{
					"type":                "server_vad",
					"silence_duration_ms": 500,
					"create_response":     true,
				},
			},
			"output": map[string]any{"voice": ct.opts.Voice},
		},
		"tools": tools,
	}
	return ct.write(ctx, map[string]any{"type": "session.update", "session": session})
}

func (ct *Control) recvLoop() {
	for {
		var msg map[string]json.RawMessage
		if err := wsjson.Read(context.Background(), ct.conn, &msg); err != nil {
			select {
			case <-ct.done:
			default:
				ct.log.Warn("sideband read error (control lost; media unaffected)", "err", err)
			}
			return
		}
		ct.mu.Lock()
		ct.lastRecv = time.Now()
		ct.mu.Unlock()

		var t string
		if raw, ok := msg["type"]; ok {
			json.Unmarshal(raw, &t) //nolint:errcheck
		}
		switch t {
		case "response.output_item.done":
			ct.handleOutputItem(msg)
		case "response.done":
			ct.parseUsage(msg)
		case "error":
			raw, _ := json.Marshal(msg)
			ct.log.Warn("sideband server error event", "raw", string(raw))
		case "session.created", "session.updated":
			ct.log.Debug("sideband session event", "type", t)
		default:
			ct.log.Debug("sideband event", "type", t)
		}
	}
}

func (ct *Control) keepalive() {
	tick := time.NewTicker(keepaliveInterval)
	defer tick.Stop()
	for {
		select {
		case <-ct.done:
			return
		case <-tick.C:
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := ct.conn.Ping(ctx)
			cancel()
			if err != nil {
				ct.log.Warn("sideband keepalive ping failed", "err", err)
				return
			}
			ct.mu.Lock()
			stale := time.Since(ct.lastRecv) > staleTimeout
			ct.mu.Unlock()
			if stale {
				ct.log.Warn("sideband stale (no events)", "timeout", staleTimeout)
			}
		}
	}
}

func (ct *Control) handleOutputItem(msg map[string]json.RawMessage) {
	itemRaw, ok := msg["item"]
	if !ok {
		return
	}
	var item struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Name   string `json:"name"`
		Args   string `json:"arguments"`
	}
	if err := json.Unmarshal(itemRaw, &item); err != nil || item.Type != "function_call" {
		return
	}

	switch item.Name {
	case "hangup_call":
		ct.log.Info("model requested hangup")
		ct.sendFunctionResult(item.CallID, `{"status":"hanging_up"}`)
		ct.sendResponseCreate(context.Background(), "Say goodbye to the caller briefly.")
		if ct.OnHangup != nil {
			ct.OnHangup()
		}
	case "transfer_call":
		var args struct {
			Destination string `json:"destination"`
		}
		if err := json.Unmarshal([]byte(item.Args), &args); err != nil {
			ct.log.Error("parse transfer args", "err", err, "args", item.Args)
			return
		}
		uri, ok := ct.opts.Transfers[args.Destination]
		if !ok {
			ct.log.Warn("unknown transfer destination", "destination", args.Destination)
			ct.sendFunctionResult(item.CallID, `{"error":"unknown destination"}`)
			return
		}
		ct.log.Info("model requested transfer", "destination", args.Destination, "uri", uri)
		ct.sendFunctionResult(item.CallID, `{"status":"transferring"}`)
		ct.sendResponseCreate(context.Background(), "Tell the caller you are transferring them now. Keep it brief.")
		if ct.OnTransfer != nil {
			ct.OnTransfer(uri)
		}
	}
}

func (ct *Control) sendResponseCreate(ctx context.Context, instructions string) {
	ct.write(ctx, map[string]any{ //nolint:errcheck
		"type":     "response.create",
		"response": map[string]any{"instructions": instructions},
	})
}

func (ct *Control) sendFunctionResult(callID, output string) {
	ct.write(context.Background(), map[string]any{ //nolint:errcheck
		"type": "conversation.item.create",
		"item": map[string]any{
			"type":    "function_call_output",
			"call_id": callID,
			"output":  output,
		},
	})
}

func (ct *Control) parseUsage(msg map[string]json.RawMessage) {
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
	ct.mu.Lock()
	ct.usage.total += u.TotalTokens
	ct.usage.input += u.InputTokens
	ct.usage.output += u.OutputTokens
	ct.usage.inText += u.InputDetail.TextTokens
	ct.usage.inAudio += u.InputDetail.AudioTokens
	ct.usage.outText += u.OutputDetail.TextTokens
	ct.usage.outAudio += u.OutputDetail.AudioTokens
	ct.mu.Unlock()
}

func (ct *Control) write(ctx context.Context, v any) error {
	ct.sendMu.Lock()
	defer ct.sendMu.Unlock()
	return wsjson.Write(ctx, ct.conn, v)
}
