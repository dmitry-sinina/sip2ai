package sip

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"sip2openai/internal/config"
	"sip2openai/internal/openai"
)

func TestCreateCallStatus(t *testing.T) {
	apiErr := func(status int) error {
		return &openai.APIError{Status: status, Body: `{"error":{"message":"Incorrect API key provided: sk-ab***cd"}}`}
	}
	cases := []struct {
		name       string
		err        error
		callerKey  bool
		wantCode   int
		wantDetail string // substring the X-Sip2ai-Error detail must contain
		hideBody   bool   // OpenAI's body must NOT reach the detail
	}{
		{"401 with caller key", apiErr(401), true, sip.StatusForbidden, "HTTP 401", false},
		{"403 with caller key", apiErr(403), true, sip.StatusForbidden, "HTTP 403", false},
		{"401 with server key", apiErr(401), false, sip.StatusBadGateway, "server credentials", true},
		{"429 with caller key", apiErr(429), true, sip.StatusTemporarilyUnavailable, "HTTP 429", false},
		{"429 with server key", apiErr(429), false, sip.StatusTemporarilyUnavailable, "HTTP 429", false},
		{"400 offer rejected", apiErr(400), true, sip.StatusNotAcceptableHere, "HTTP 400", false},
		{"422 offer rejected", apiErr(422), false, sip.StatusNotAcceptableHere, "HTTP 422", false},
		{"500 upstream", apiErr(500), true, sip.StatusBadGateway, "HTTP 500", false},
		{"transport error", errors.New("dial tcp: connection refused"), true, sip.StatusBadGateway, "connection refused", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, reason, detail := createCallStatus(tc.err, tc.callerKey)
			if code != tc.wantCode {
				t.Errorf("code = %d %q, want %d", code, reason, tc.wantCode)
			}
			if reason == "" {
				t.Error("empty reason phrase")
			}
			if detail == nil || !strings.Contains(detail.Error(), tc.wantDetail) {
				t.Errorf("detail = %v, want it to contain %q", detail, tc.wantDetail)
			}
			if tc.hideBody && strings.Contains(detail.Error(), "sk-ab") {
				t.Errorf("detail %q leaks OpenAI's echo of the server key", detail)
			}
		})
	}
}

// TestEndCallHangsUpWithCallKey asserts the teardown path (endCall, as run on a
// caller BYE or a model hangup) ends the OpenAI leg with the client the call
// was created with, i.e. under the caller's api_key rather than the server's.
func TestEndCallHangsUpWithCallKey(t *testing.T) {
	authCh := make(chan string, 1)
	oaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/realtime/calls/rtc_1/hangup" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		authCh <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer oaiSrv.Close()

	base, err := openai.New("sk-server", "gpt-realtime", oaiSrv.URL, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := &Server{
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		oai:   base,
		calls: map[string]*activeCall{},
	}
	s.calls["call-1"] = &activeCall{oai: base.WithAPIKey("sk-caller"), oaiCallID: "rtc_1"}

	s.endCall("call-1", false)

	select {
	case auth := <-authCh:
		if auth != "Bearer sk-caller" {
			t.Errorf("hangup Authorization = %q, want Bearer sk-caller", auth)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OpenAI hangup never sent")
	}
	if _, still := s.calls["call-1"]; still {
		t.Error("call still registered after endCall")
	}
}

// TestInviteRejectedKeyMapsToForbidden drives a real INVITE carrying an
// api_key that OpenAI rejects (HTTP 401) through onInvite and asserts the
// caller gets 403 with OpenAI's details in X-Sip2ai-Error, not the generic 488
// media rejection, and that the offer went out under the caller's key.
func TestInviteRejectedKeyMapsToForbidden(t *testing.T) {
	sip.SetDefaultLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	authCh := make(chan string, 1)
	oaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authCh <- r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	defer oaiSrv.Close()
	base, err := openai.New("sk-server", "gpt-realtime", oaiSrv.URL, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// --- UAS (our side): onInvite runs up to and including CreateCall. ---
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	uasUA, _ := sipgo.NewUA(sipgo.WithUserAgent("sip2openai"))
	uasCli, _ := sipgo.NewClient(uasUA)
	uasSrv, _ := sipgo.NewServer(uasUA)
	uasPC, uasPort := bindUDP(t)
	uasContact := sip.ContactHeader{Address: sip.Uri{User: "sip2openai", Host: "127.0.0.1", Port: uasPort}}
	srvUnderTest := &Server{
		oaiCfg:  config.OpenAIConfig{APIKey: "sk-server", Model: "gpt-realtime"},
		log:     discard,
		oaiLog:  discard,
		oai:     base,
		dlg:     sipgo.NewDialogServerCache(uasCli, uasContact),
		contact: uasContact,
		calls:   map[string]*activeCall{},
	}
	uasSrv.OnInvite(srvUnderTest.onInvite)
	serve(uasSrv, uasPC)

	// --- UAC (caller): sends the INVITE and reads the final response. ---
	uacUA, _ := sipgo.NewUA(sipgo.WithUserAgent("uac"))
	uacCli, _ := sipgo.NewClient(uacUA)
	_, uacPort := bindUDP(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	uasURI := sip.Uri{User: "sip2openai", Host: "127.0.0.1", Port: uasPort}
	req := sip.NewRequest(sip.INVITE, uasURI)
	req.SetDestination(uasURI.HostPort())
	req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "caller", Host: "127.0.0.1", Port: uacPort}})
	req.SetBody([]byte("v=0\r\no=- 0 0 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\nm=audio 2 RTP/AVP 0\r\n"))
	req.AppendHeader(sip.NewHeader(configHeader, `{"api_key":"sk-revoked"}`))

	tx, err := uacCli.TransactionRequest(ctx, req)
	if err != nil {
		t.Fatalf("TransactionRequest: %v", err)
	}
	defer tx.Terminate()

	var resp *sip.Response
	for resp == nil || resp.StatusCode < 200 { // skip 100 Trying
		select {
		case resp = <-tx.Responses():
		case <-time.After(3 * time.Second):
			t.Fatal("no final response to INVITE")
		}
	}

	if resp.StatusCode != sip.StatusForbidden {
		t.Fatalf("status = %d %q, want 403", resp.StatusCode, resp.Reason)
	}
	h := resp.GetHeader(errorHeader)
	if h == nil {
		t.Fatalf("missing %s header", errorHeader)
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(h.Value()), &m); err != nil {
		t.Fatalf("%s value is not valid JSON: %q: %v", errorHeader, h.Value(), err)
	}
	if !strings.Contains(m["error"], "HTTP 401") {
		t.Errorf("%s = %q, want OpenAI's 401 in the details", errorHeader, h.Value())
	}

	select {
	case auth := <-authCh:
		if auth != "Bearer sk-revoked" {
			t.Errorf("offer Authorization = %q, want Bearer sk-revoked", auth)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OpenAI never received the offer")
	}
	if n := len(srvUnderTest.calls); n != 0 {
		t.Errorf("%d call(s) registered after a rejected INVITE, want 0", n)
	}
}
