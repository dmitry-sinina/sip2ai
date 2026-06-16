package openai

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"
)

func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// wsServer starts an httptest server that upgrades /v1/realtime to a WebSocket
// and hands the connection to onConn. onConn must block until it is done; the
// connection closes when it returns.
func wsServer(t *testing.T, onConn func(ctx context.Context, c *websocket.Conn)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/realtime", func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("ws accept: %v", err)
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")
		onConn(r.Context(), c)
	})
	return httptest.NewServer(mux)
}

// emitEvent pushes one function_call item to the client after consuming the
// session.update that Start sends. su (if non-nil) receives the session.update.
func emitEvent(su chan<- map[string]any, name, args string) func(context.Context, *websocket.Conn) {
	return func(ctx context.Context, c *websocket.Conn) {
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
		for {
			if _, _, err := c.Read(ctx); err != nil {
				return
			}
		}
	}
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
