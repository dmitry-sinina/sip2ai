package sip

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"sip2openai/internal/openai"
)

func TestUsageHeaderValue(t *testing.T) {
	t.Run("serializes every counter with OpenAI's field names", func(t *testing.T) {
		got := usageHeaderValue(openai.Usage{
			Total: 100, Input: 60, Output: 40,
			InText: 10, InAudio: 50, OutText: 5, OutAudio: 35,
		})
		var m map[string]int
		if err := json.Unmarshal([]byte(got), &m); err != nil {
			t.Fatalf("not valid JSON: %q: %v", got, err)
		}
		want := map[string]int{
			"total_tokens": 100, "input_tokens": 60, "output_tokens": 40,
			"input_text_tokens": 10, "input_audio_tokens": 50,
			"output_text_tokens": 5, "output_audio_tokens": 35,
		}
		for k, v := range want {
			if m[k] != v {
				t.Errorf("%s = %d, want %d", k, m[k], v)
			}
		}
		if len(m) != len(want) {
			t.Errorf("got %d fields, want %d: %q", len(m), len(want), got)
		}
	})

	t.Run("zero usage is a full object of zeros", func(t *testing.T) {
		got := usageHeaderValue(openai.Usage{})
		if got != `{"total_tokens":0,"input_tokens":0,"output_tokens":0,"input_text_tokens":0,"input_audio_tokens":0,"output_text_tokens":0,"output_audio_tokens":0}` {
			t.Errorf("value = %q", got)
		}
	})
}

func TestUsageHeaderFor(t *testing.T) {
	t.Run("nil call reports nothing", func(t *testing.T) {
		if h := usageHeaderFor(nil); h != nil {
			t.Errorf("got %v, want nil", h)
		}
	})

	t.Run("call without sideband control reports nothing", func(t *testing.T) {
		if h := usageHeaderFor(&activeCall{oaiCallID: "rtc_x"}); h != nil {
			t.Errorf("got %v, want nil", h)
		}
	})

	t.Run("call with control reports its totals", func(t *testing.T) {
		ctrl := (&openai.Client{}).NewControl("rtc_x", openai.ControlOptions{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
		h := usageHeaderFor(&activeCall{ctrl: ctrl})
		if h == nil {
			t.Fatal("got nil header, want one")
		}
		if h.Name() != usageHeader {
			t.Errorf("name = %q, want %q", h.Name(), usageHeader)
		}
		if err := json.Unmarshal([]byte(h.Value()), &map[string]int{}); err != nil {
			t.Errorf("value is not valid JSON: %q: %v", h.Value(), err)
		}
	})
}

// captureTx is a ServerTransaction that only records the response it is given.
// The embedded nil interface panics on any other method, none of which the
// header wrapper calls.
type captureTx struct {
	sip.ServerTransaction
	got *sip.Response
}

func (c *captureTx) Respond(res *sip.Response) error {
	c.got = res
	return nil
}

func TestHeaderTx(t *testing.T) {
	req := sip.NewRequest(sip.BYE, sip.Uri{User: "caller", Host: "127.0.0.1", Port: 5060})
	rec := &captureTx{}
	tx := &headerTx{ServerTransaction: rec, extra: []sip.Header{sip.NewHeader(usageHeader, `{"total_tokens":7}`)}}

	if err := tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if rec.got == nil {
		t.Fatal("response never reached the wrapped transaction")
	}
	h := rec.got.GetHeader(usageHeader)
	if h == nil || h.Value() != `{"total_tokens":7}` {
		t.Errorf("%s = %v, want the injected value", usageHeader, h)
	}
	if rec.got.StatusCode != sip.StatusOK {
		t.Errorf("status = %d, want 200", rec.got.StatusCode)
	}
}

// TestByeUsageHeaderRoundTrip drives a real INVITE/200/ACK handshake, then has
// the UAS send the teardown BYE and asserts the usage header on the wire.
func TestByeUsageHeaderRoundTrip(t *testing.T) {
	sip.SetDefaultLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// --- UAC (caller): places the call and must serve to receive the BYE. ---
	uacUA, _ := sipgo.NewUA(sipgo.WithUserAgent("uac"))
	uacCli, _ := sipgo.NewClient(uacUA)
	uacSrv, _ := sipgo.NewServer(uacUA)
	uacPC, uacPort := bindUDP(t)
	uacContact := sip.ContactHeader{Address: sip.Uri{User: "caller", Host: "127.0.0.1", Port: uacPort}}
	dcc := sipgo.NewDialogClientCache(uacCli, uacContact)

	byeCh := make(chan *sip.Request, 1)
	uacSrv.OnBye(func(req *sip.Request, tx sip.ServerTransaction) {
		byeCh <- req
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	})
	serve(uacSrv, uacPC)

	// --- UAS (our side): answers the INVITE, then tears the call down. ---
	uasUA, _ := sipgo.NewUA(sipgo.WithUserAgent("sip2openai"))
	uasCli, _ := sipgo.NewClient(uasUA)
	uasSrv, _ := sipgo.NewServer(uasUA)
	uasPC, uasPort := bindUDP(t)
	uasContact := sip.ContactHeader{Address: sip.Uri{User: "sip2openai", Host: "127.0.0.1", Port: uasPort}}
	dsc := sipgo.NewDialogServerCache(uasCli, uasContact)

	dlgCh := make(chan *sipgo.DialogServerSession, 1)
	ackCh := make(chan struct{}, 1)
	uasSrv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		dlg, err := dsc.ReadInvite(req, tx)
		if err != nil {
			t.Errorf("ReadInvite: %v", err)
			return
		}
		if err := dlg.RespondSDP([]byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 1 RTP/AVP 0\r\n")); err != nil {
			t.Errorf("RespondSDP: %v", err)
			return
		}
		dlgCh <- dlg
	})
	uasSrv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = dsc.ReadAck(req, tx)
		ackCh <- struct{}{}
	})
	serve(uasSrv, uasPC)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	uasURI := sip.Uri{User: "sip2openai", Host: "127.0.0.1", Port: uasPort}
	uacDlg, err := dcc.Invite(ctx, uasURI, []byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 2 RTP/AVP 0\r\n"))
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if err := uacDlg.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := uacDlg.Ack(ctx); err != nil {
		t.Fatalf("Ack: %v", err)
	}

	var uasDlg *sipgo.DialogServerSession
	select {
	case uasDlg = <-dlgCh:
	case <-time.After(3 * time.Second):
		t.Fatal("UAS never received INVITE")
	}
	select {
	case <-ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("UAS never received ACK (dialog not confirmed)")
	}

	// The thing under test: BYE carrying the usage header.
	want := usageHeaderValue(openai.Usage{Total: 100, Input: 60, Output: 40, InAudio: 50, OutAudio: 35})
	if err := sendByeWithHeaders(ctx, uasDlg, sip.NewHeader(usageHeader, want)); err != nil {
		t.Fatalf("sendByeWithHeaders: %v", err)
	}

	select {
	case bye := <-byeCh:
		if bye.Method != sip.BYE {
			t.Errorf("method = %v, want BYE", bye.Method)
		}
		h := bye.GetHeader(usageHeader)
		if h == nil {
			t.Fatalf("%s missing from BYE", usageHeader)
		}
		if h.Value() != want {
			t.Errorf("%s = %q, want %q", usageHeader, h.Value(), want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UAC never received BYE")
	}
}

// TestCallerByeUsageHeaderRoundTrip drives a caller-initiated BYE into our
// onBye handler and asserts the usage header on the 200 OK it sends back —
// that response is built inside sipgo, so this covers the headerTx wrapping.
func TestCallerByeUsageHeaderRoundTrip(t *testing.T) {
	sip.SetDefaultLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// --- UAC (caller): places the call, then sends the BYE. ---
	uacUA, _ := sipgo.NewUA(sipgo.WithUserAgent("uac"))
	uacCli, _ := sipgo.NewClient(uacUA)
	uacSrv, _ := sipgo.NewServer(uacUA)
	uacPC, uacPort := bindUDP(t)
	uacContact := sip.ContactHeader{Address: sip.Uri{User: "caller", Host: "127.0.0.1", Port: uacPort}}
	dcc := sipgo.NewDialogClientCache(uacCli, uacContact)
	serve(uacSrv, uacPC)

	// --- UAS (our side): answers, registers the call, then handles the BYE. ---
	uasUA, _ := sipgo.NewUA(sipgo.WithUserAgent("sip2openai"))
	uasCli, _ := sipgo.NewClient(uasUA)
	uasSrv, _ := sipgo.NewServer(uasUA)
	uasPC, uasPort := bindUDP(t)
	uasContact := sip.ContactHeader{Address: sip.Uri{User: "sip2openai", Host: "127.0.0.1", Port: uasPort}}
	dsc := sipgo.NewDialogServerCache(uasCli, uasContact)

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	// oaiCallID is empty so teardown's OpenAI hangup is a no-op (no client).
	srvUnderTest := &Server{
		dlg:     dsc,
		contact: uasContact,
		log:     discard,
		calls:   make(map[string]*activeCall),
	}

	ackCh := make(chan struct{}, 1)
	uasSrv.OnInvite(func(req *sip.Request, tx sip.ServerTransaction) {
		dlg, err := dsc.ReadInvite(req, tx)
		if err != nil {
			t.Errorf("ReadInvite: %v", err)
			return
		}
		if err := dlg.RespondSDP([]byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 1 RTP/AVP 0\r\n")); err != nil {
			t.Errorf("RespondSDP: %v", err)
			return
		}
		ctrl := (&openai.Client{}).NewControl("", openai.ControlOptions{}, discard)
		srvUnderTest.mu.Lock()
		srvUnderTest.calls[req.CallID().Value()] = &activeCall{dlg: dlg, ctrl: ctrl}
		srvUnderTest.mu.Unlock()
	})
	uasSrv.OnAck(func(req *sip.Request, tx sip.ServerTransaction) {
		_ = dsc.ReadAck(req, tx)
		ackCh <- struct{}{}
	})
	uasSrv.OnBye(srvUnderTest.onBye)
	serve(uasSrv, uasPC)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	uasURI := sip.Uri{User: "sip2openai", Host: "127.0.0.1", Port: uasPort}
	uacDlg, err := dcc.Invite(ctx, uasURI, []byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 2 RTP/AVP 0\r\n"))
	if err != nil {
		t.Fatalf("Invite: %v", err)
	}
	if err := uacDlg.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
		t.Fatalf("WaitAnswer: %v", err)
	}
	if err := uacDlg.Ack(ctx); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	select {
	case <-ackCh:
	case <-time.After(3 * time.Second):
		t.Fatal("UAS never received ACK (dialog not confirmed)")
	}

	// Caller hangs up; the 200 OK must carry the usage totals.
	bye := sip.NewRequest(sip.BYE, uasContact.Address)
	res, err := uacDlg.Do(ctx, bye)
	if err != nil {
		t.Fatalf("send BYE: %v", err)
	}
	if res.StatusCode != sip.StatusOK {
		t.Fatalf("BYE response = %d %s, want 200", res.StatusCode, res.Reason)
	}
	h := res.GetHeader(usageHeader)
	if h == nil {
		t.Fatalf("%s missing from 200 OK to BYE", usageHeader)
	}
	var m map[string]int
	if err := json.Unmarshal([]byte(h.Value()), &m); err != nil {
		t.Fatalf("%s is not valid JSON: %q: %v", usageHeader, h.Value(), err)
	}
	if _, ok := m["total_tokens"]; !ok {
		t.Errorf("%s = %q, want total_tokens present", usageHeader, h.Value())
	}
}
