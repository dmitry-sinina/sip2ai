package openai

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// wsServer starts an httptest server that upgrades /v1/realtime to a WebSocket
// and hands the connection and the handshake request to onConn. onConn must
// block until it is done; the connection closes when it returns.
func wsServer(t *testing.T, onConn func(ctx context.Context, r *http.Request, c *websocket.Conn)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/realtime", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("ws accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		onConn(r.Context(), r, c)
	})
	return httptest.NewServer(mux)
}

// drain reads and discards client frames until the control closes the
// connection, keeping the server side of a wsServer alive meanwhile.
func drain(ctx context.Context, c *websocket.Conn) {
	c.SetReadLimit(-1)
	for {
		if _, _, err := c.Read(ctx); err != nil {
			return
		}
	}
}

// emitEvent pushes one function_call item to the client after consuming the
// session.update that Start sends. su (if non-nil) receives the session.update.
func emitEvent(su chan<- map[string]any, name, args string) func(context.Context, *http.Request, *websocket.Conn) {
	return func(ctx context.Context, _ *http.Request, c *websocket.Conn) {
		c.SetReadLimit(-1)
		var got map[string]any
		if err := wsjson.Read(ctx, c, &got); err != nil {
			return
		}
		if su != nil {
			su <- got
		}
		_ = wsjson.Write(ctx, c, map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{
				"type":      "function_call",
				"name":      name,
				"call_id":   "fc_1",
				"arguments": args,
			},
		})
		// Drain client writes (function_call_output / response.create) and
		// return as soon as the control closes the connection.
		drain(ctx, c)
	}
}

// timedMsg is a client frame as recorded by recordAll, with its arrival time.
type timedMsg struct {
	msg map[string]any
	at  time.Time
}

// recordAll forwards every client frame to msgs until the control closes the
// connection. It sends nothing back.
func recordAll(msgs chan<- timedMsg) func(context.Context, *http.Request, *websocket.Conn) {
	return func(ctx context.Context, _ *http.Request, c *websocket.Conn) {
		c.SetReadLimit(-1)
		for {
			var got map[string]any
			if err := wsjson.Read(ctx, c, &got); err != nil {
				return
			}
			msgs <- timedMsg{msg: got, at: time.Now()}
		}
	}
}

// greetingText extracts response.instructions from a response.create frame.
func greetingText(m map[string]any) string {
	resp, _ := m["response"].(map[string]any)
	s, _ := resp["instructions"].(string)
	return s
}

func newTestControl(t *testing.T, srvURL string, opts ControlOptions) *Control {
	t.Helper()
	c, err := New("sk-test", "gpt-realtime", srvURL, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c.NewControl("rtc_1", opts, discardLogger())
}

func TestControlSessionUpdateAndTransfer(t *testing.T) {
	su := make(chan map[string]any, 1)
	srv := wsServer(t, emitEvent(su, "transfer_call", `{"destination":"Dave"}`))
	defer srv.Close()

	ctrl := newTestControl(t, srv.URL, ControlOptions{
		Voice:        "alloy",
		Instructions: "be nice",
		HangupDesc:   "hang up",
		TransferDesc: "transfer",
		Transfers:    map[string]string{"Dave": "tel:42"},
	})
	transferCh := make(chan string, 1)
	ctrl.OnTransfer = func(uri string) { transferCh <- uri }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ctrl.Close()

	// session.update sent on Start, with both tools (Transfers is non-empty).
	select {
	case msg := <-su:
		if msg["type"] != "session.update" {
			t.Errorf("first message type = %v, want session.update", msg["type"])
		}
		sess, _ := msg["session"].(map[string]any)
		tools, _ := sess["tools"].([]any)
		if len(tools) != 2 {
			t.Errorf("tools = %d, want 2 (hangup_call + transfer_call)", len(tools))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no session.update received")
	}

	// transfer_call resolves "Dave" -> normalized URI and fires OnTransfer.
	select {
	case uri := <-transferCh:
		if uri != "tel:42" {
			t.Errorf("OnTransfer uri = %q, want tel:42", uri)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnTransfer not called")
	}
}

func TestControlHangupDispatch(t *testing.T) {
	srv := wsServer(t, emitEvent(nil, "hangup_call", `{}`))
	defer srv.Close()

	ctrl := newTestControl(t, srv.URL, ControlOptions{Voice: "alloy", HangupDesc: "hang up"})
	hangupCh := make(chan struct{}, 1)
	ctrl.OnHangup = func() { hangupCh <- struct{}{} }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ctrl.Close()

	select {
	case <-hangupCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnHangup not called")
	}
}

func TestControlUnknownTransferDestination(t *testing.T) {
	srv := wsServer(t, emitEvent(nil, "transfer_call", `{"destination":"Nobody"}`))
	defer srv.Close()

	ctrl := newTestControl(t, srv.URL, ControlOptions{
		Transfers: map[string]string{"Dave": "tel:42"},
	})
	called := make(chan string, 1)
	ctrl.OnTransfer = func(uri string) { called <- uri }

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ctrl.Close()

	select {
	case uri := <-called:
		t.Errorf("OnTransfer should not fire for unknown destination, got %q", uri)
	case <-time.After(500 * time.Millisecond):
		// expected: no callback
	}
}

// TestControlGreetAfterDelay asserts Start itself never speaks the greeting,
// and GreetAfter sends the response.create only once the delay has elapsed.
func TestControlGreetAfterDelay(t *testing.T) {
	msgs := make(chan timedMsg, 16)
	srv := wsServer(t, recordAll(msgs))
	defer srv.Close()

	ctrl := newTestControl(t, srv.URL, ControlOptions{Voice: "alloy", Greeting: "Hi there"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ctrl.Close()

	select {
	case m := <-msgs:
		if m.msg["type"] != "session.update" {
			t.Fatalf("first message type = %v, want session.update", m.msg["type"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no session.update received")
	}

	const delay = 300 * time.Millisecond
	scheduled := time.Now()
	ctrl.GreetAfter(delay)

	// Nothing may be sent before the delay is up (Start's greeting used to
	// follow session.update immediately — that is the regression guarded here).
	select {
	case m := <-msgs:
		t.Fatalf("got %v only %v after GreetAfter, want nothing before %v", m.msg["type"], m.at.Sub(scheduled), delay)
	case <-time.After(delay - 100*time.Millisecond):
	}

	select {
	case m := <-msgs:
		if m.msg["type"] != "response.create" {
			t.Fatalf("message type = %v, want response.create", m.msg["type"])
		}
		if got := m.at.Sub(scheduled); got < delay {
			t.Errorf("greeting sent %v after GreetAfter, want >= %v", got, delay)
		}
		if instr := greetingText(m.msg); !strings.Contains(instr, "Hi there") {
			t.Errorf("response.create instructions = %q, want the greeting text", instr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("greeting never sent")
	}
}

// TestControlGreetSkippedWhenCallerSpoke asserts a pending greeting is dropped
// once server VAD has reported caller speech, so the scripted greeting does
// not pile onto the response the model already owes the caller.
func TestControlGreetSkippedWhenCallerSpoke(t *testing.T) {
	msgs := make(chan timedMsg, 16)
	srv := wsServer(t, func(ctx context.Context, r *http.Request, c *websocket.Conn) {
		c.SetReadLimit(-1)
		var got map[string]any
		if err := wsjson.Read(ctx, c, &got); err != nil { // session.update
			return
		}
		msgs <- timedMsg{msg: got, at: time.Now()}
		_ = wsjson.Write(ctx, c, map[string]any{"type": "input_audio_buffer.speech_started"})
		recordAll(msgs)(ctx, r, c)
	})
	defer srv.Close()

	ctrl := newTestControl(t, srv.URL, ControlOptions{Voice: "alloy", Greeting: "Hi there"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ctrl.Close()
	<-msgs // session.update

	// Wait for recvLoop to have seen speech_started before scheduling.
	deadline := time.Now().Add(2 * time.Second)
	for {
		ctrl.mu.Lock()
		spoke := ctrl.callerSpoke
		ctrl.mu.Unlock()
		if spoke {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("speech_started never registered")
		}
		time.Sleep(10 * time.Millisecond)
	}

	ctrl.GreetAfter(0)
	select {
	case m := <-msgs:
		t.Errorf("got %v after the caller spoke, want no greeting", m.msg["type"])
	case <-time.After(500 * time.Millisecond):
	}
}

// TestControlGreetAfterNoGreeting asserts GreetAfter is a no-op without a
// configured greeting.
func TestControlGreetAfterNoGreeting(t *testing.T) {
	msgs := make(chan timedMsg, 16)
	srv := wsServer(t, recordAll(msgs))
	defer srv.Close()

	ctrl := newTestControl(t, srv.URL, ControlOptions{Voice: "alloy"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ctrl.Close()
	<-msgs // session.update

	ctrl.GreetAfter(0)
	select {
	case m := <-msgs:
		t.Errorf("got %v, want nothing without a greeting", m.msg["type"])
	case <-time.After(300 * time.Millisecond):
	}
}

// TestControlDialUsesClientAPIKey asserts the sideband WS handshake carries the
// key of the client NewControl was called on, so a per-call WithAPIKey client
// authenticates the control plane with the caller's key.
func TestControlDialUsesClientAPIKey(t *testing.T) {
	authCh := make(chan string, 1)
	srv := wsServer(t, func(ctx context.Context, r *http.Request, c *websocket.Conn) {
		authCh <- r.Header.Get("Authorization")
		drain(ctx, c)
	})
	defer srv.Close()

	base, err := New("sk-server", "gpt-realtime", srv.URL, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctrl := base.WithAPIKey("sk-caller").NewControl("rtc_1", ControlOptions{Voice: "alloy"}, discardLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ctrl.Close()

	select {
	case auth := <-authCh:
		if auth != "Bearer sk-caller" {
			t.Errorf("sideband Authorization = %q, want Bearer sk-caller", auth)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sideband WS never dialed")
	}
}
