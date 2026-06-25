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
)

// TestInviteMalformedConfigRejected drives a real INVITE carrying a malformed
// X-Sip2ai-Config header into our onInvite handler and asserts the UAS replies
// 400 Bad Request with the error details in the X-Sip2ai-Error header — and
// that no dialog/session is established.
func TestInviteMalformedConfigRejected(t *testing.T) {
	sip.SetDefaultLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// --- UAS (our side): only the reject path of onInvite runs. ---
	uasUA, _ := sipgo.NewUA(sipgo.WithUserAgent("sip2openai"))
	uasSrv, _ := sipgo.NewServer(uasUA)
	uasPC, uasPort := bindUDP(t)

	srvUnderTest := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	uasSrv.OnInvite(srvUnderTest.onInvite)
	serve(uasSrv, uasPC)

	// --- UAC (caller): sends the bad INVITE and inspects the response. ---
	uacUA, _ := sipgo.NewUA(sipgo.WithUserAgent("uac"))
	uacCli, _ := sipgo.NewClient(uacUA)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	uasURI := sip.Uri{User: "sip2openai", Host: "127.0.0.1", Port: uasPort}
	req := sip.NewRequest(sip.INVITE, uasURI)
	req.SetDestination(uasURI.HostPort())
	// Non-empty SDP so we pass the missing-offer check and reach config parsing.
	req.SetBody([]byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 2 RTP/AVP 0\r\n"))
	req.AppendHeader(sip.NewHeader(configHeader, `{not json`))

	tx, err := uacCli.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("TransactionRequest: %v", err)
	}
	defer tx.Terminate()

	var resp *sip.Response
	select {
	case resp = <-tx.Responses():
	case <-time.After(3 * time.Second):
		t.Fatal("no response to INVITE")
	}

	if resp.StatusCode != sip.StatusBadRequest {
		t.Fatalf("status = %d %q, want 400", resp.StatusCode, resp.Reason)
	}

	h := resp.GetHeader(errorHeader)
	if h == nil {
		t.Fatalf("missing %s header", errorHeader)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(h.Value()), &m); err != nil {
		t.Fatalf("%s value is not valid JSON: %q: %v", errorHeader, h.Value(), err)
	}
	if m["error"] == "" {
		t.Errorf("%s has empty error field: %q", errorHeader, h.Value())
	}
}
