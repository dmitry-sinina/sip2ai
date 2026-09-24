// Package sip is the SIP UAS for sip2openai. It terminates signaling only:
// inbound INVITE offers are relayed to OpenAI and the answer is returned, while
// media (ICE/DTLS-SRTP/Opus or G.711) flows directly between caller and OpenAI.
// A per-call sideband WebSocket carries session config and tool calls.
package sip

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"sip2openai/internal/config"
	"sip2openai/internal/openai"
	sdpx "sip2openai/internal/sdp"
)

// configHeader is the SIP header carrying a per-call JSON config override.
// Header lookup is case-insensitive, so "X-SIP2AI-CONFIG" also matches.
const configHeader = "X-Sip2ai-Config"

// errorHeader carries JSON-serialized error details on rejection responses
// (e.g. a malformed configHeader), so the caller can see why the INVITE failed.
const errorHeader = "X-Sip2ai-Error"

// usageHeader carries the call's JSON token totals. The totals are only final
// when the call ends, so it rides on call teardown — the BYE we send on a
// model-initiated hangup, and our 200 OK to a caller-initiated BYE.
const usageHeader = "X-Sip2ai-Usage"

// activeCall holds the per-call resources we must tear down together.
type activeCall struct {
	dlg       *sipgo.DialogServerSession
	ctrl      *openai.Control
	oaiCallID string
	// oai is the client the call was created with. It differs from the
	// server's when the caller supplied its own api_key, and the hangup must
	// go out under the same credentials.
	oai *openai.Client
}

// Server is a signaling-only UAS bridging SIP calls to OpenAI Realtime.
type Server struct {
	sipCfg    config.SIPConfig
	oaiCfg    config.OpenAIConfig
	transfers map[string]string
	log       *slog.Logger // app-level SIP handler events
	oaiLog    *slog.Logger // OpenAI sideband signaling (own log level)
	oai       *openai.Client

	ua      *sipgo.UserAgent
	srv     *sipgo.Server
	cli     *sipgo.Client
	dlg     *sipgo.DialogServerCache
	contact sip.ContactHeader // our Contact, used as Referred-By on REFER

	mu    sync.Mutex
	calls map[string]*activeCall // SIP Call-ID -> resources
}

// New wires the sipgo UA/server/dialog cache and registers handlers.
func New(sipCfg config.SIPConfig, oaiCfg config.OpenAIConfig, transfers map[string]string, oai *openai.Client, log, oaiLog *slog.Logger) (*Server, error) {
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("sip2openai"))
	if err != nil {
		return nil, fmt.Errorf("new ua: %w", err)
	}
	cli, err := sipgo.NewClient(ua)
	if err != nil {
		return nil, fmt.Errorf("new client: %w", err)
	}
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		return nil, fmt.Errorf("new server: %w", err)
	}

	host := sipCfg.ExternalHost
	if host == "" {
		host = sipCfg.BindHost
	}
	port := sipCfg.ExternalPort
	if port == 0 {
		port = sipCfg.BindPort
	}
	contact := sip.ContactHeader{Address: sip.Uri{User: "sip2openai", Host: host, Port: port}}

	s := &Server{
		sipCfg:    sipCfg,
		oaiCfg:    oaiCfg,
		transfers: transfers,
		log:       log,
		oaiLog:    oaiLog,
		oai:       oai,
		ua:        ua,
		srv:       srv,
		cli:       cli,
		dlg:       sipgo.NewDialogServerCache(cli, contact),
		contact:   contact,
		calls:     make(map[string]*activeCall),
	}
	srv.OnInvite(s.onInvite)
	srv.OnAck(s.onAck)
	srv.OnBye(s.onBye)
	srv.OnRefer(s.onRefer)   // reject inbound REFER; we only send them
	srv.OnNotify(s.onNotify) // consume REFER-progress sipfrag NOTIFYs
	return s, nil
}

// Run listens on UDP (always) and TCP (if enabled) until ctx is cancelled.
func (s *Server) Run(ctx context.Context) error {
	addr := fmt.Sprintf("%s:%d", s.sipCfg.BindHost, s.sipCfg.BindPort)
	errCh := make(chan error, 2)

	go func() {
		s.log.Info("SIP listening", "transport", "udp", "addr", addr)
		errCh <- s.srv.ListenAndServe(ctx, "udp", addr)
	}()
	if s.sipCfg.EnableTCP {
		go func() {
			s.log.Info("SIP listening", "transport", "tcp", "addr", addr)
			errCh <- s.srv.ListenAndServe(ctx, "tcp", addr)
		}()
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (s *Server) onInvite(req *sip.Request, tx sip.ServerTransaction) {
	sipCallID := req.CallID().Value()
	log := s.log.With("callid", sipCallID)

	offer := req.Body()
	if len(offer) == 0 {
		log.Warn("INVITE without SDP offer")
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Missing SDP", nil))
		return
	}

	// Per-call config override from the X-Sip2ai-Config header (if present).
	// A malformed header is a client error: reject the INVITE rather than
	// silently falling back to server config and starting a session.
	oaiCfg, transfers := s.oaiCfg, s.transfers
	override, err := parseConfigHeader(req)
	if err != nil {
		log.Warn("bad "+configHeader+" header, rejecting INVITE", "err", err)
		resp := sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil)
		resp.AppendHeader(sip.NewHeader(errorHeader, errorDetailsJSON(err)))
		_ = tx.Respond(resp)
		return
	}
	if override != nil {
		eff := config.Config{OpenAI: s.oaiCfg, Transfers: s.transfers}.WithOverride(override)
		oaiCfg, transfers = eff.OpenAI, eff.Transfers
		log.Info("per-call config override applied", "model", oaiCfg.Model, "voice", oaiCfg.Voice,
			"transfer_dests", len(transfers), "api_key_override", override.APIKey != nil)
	}
	// Per-call client: the server's unless the caller overrode api_key, in
	// which case every OpenAI request for this call (offer, sideband, hangup)
	// authenticates with the effective (validated, trimmed) key instead. The
	// fallback to server credentials is decided here and nowhere else.
	callerKey := override != nil && override.APIKey != nil
	oai := s.oai
	if callerKey {
		oai = s.oai.WithAPIKey(oaiCfg.APIKey)
	}

	dlg, err := s.dlg.ReadInvite(req, tx)
	if err != nil {
		log.Error("ReadInvite failed", "err", err)
		return
	}
	_ = dlg.Respond(sip.StatusTrying, "Trying", nil)

	// Make the telephony offer JSEP-idiomatic, then relay to OpenAI.
	normOffer := sdpx.EnsureBundleMid(offer)
	answer, oaiCallID, err := oai.CreateCall(dlg.Context(), normOffer, oaiCfg.Model)
	if err != nil {
		code, reason, detail := createCallStatus(err, callerKey)
		log.Error("CreateCall failed", "err", err, "status", code)
		_ = dlg.Respond(code, reason, nil, sip.NewHeader(errorHeader, errorDetailsJSON(detail)))
		return
	}
	log.Info("OpenAI call created", "openai_call_id", oaiCallID, "answer_bytes", len(answer))

	// Strip the mid/BUNDLE we injected so the answer stays symmetric with the
	// original offer, then return it in the 200 OK.
	answerForClient := sdpx.StripBundleMid(answer)
	if err := dlg.RespondSDP(answerForClient); err != nil {
		log.Error("send 200 OK failed", "err", err, "bytes", len(answerForClient))
		hangupOpenAI(oai, oaiCallID, log)
		return
	}
	// RespondSDP retransmits the 200 OK until the caller's ACK and returns
	// only then, so this is the ACK time: the earliest the caller can have
	// started ICE/DTLS towards OpenAI, and the reference for the greeting delay.
	ackedAt := time.Now()
	log.Info("call answered (ACKed) — media direct to OpenAI", "answer_bytes", len(answerForClient))

	// Register the call, then bring up the sideband control plane.
	ac := &activeCall{dlg: dlg, oaiCallID: oaiCallID, oai: oai}
	ctrl := oai.NewControl(oaiCallID, s.controlOpts(oaiCfg, transfers), s.oaiLog.With("callid", sipCallID))
	ctrl.OnHangup = func() { s.endCall(sipCallID, true) }
	ctrl.OnTransfer = func(uri string) {
		// Fires from the sideband recv goroutine. Blind (unattended) transfer:
		// REFER the caller to the destination; the caller places the new call
		// directly and then tears our leg down (BYE / final sipfrag NOTIFY).
		rctx, rcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer rcancel()
		if err := s.sendRefer(rctx, dlg, uri); err != nil {
			log.Error("SIP REFER failed", "err", err, "destination", uri)
			return
		}
		log.Info("SIP REFER sent", "destination", uri)
	}
	ac.ctrl = ctrl

	s.mu.Lock()
	s.calls[sipCallID] = ac
	s.mu.Unlock()

	dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	startErr := ctrl.Start(dctx)
	cancel()
	if startErr != nil {
		log.Warn("sideband control failed to start (call continues, no prompt/greeting)", "err", startErr)
		return
	}
	log.Info("sideband control up", "greeting_delay_ms", oaiCfg.GreetingDelayMs)
	// Hold the greeting until greeting_delay_ms after the ACK: media is set up
	// caller<->OpenAI out of our sight, and speaking before it is up clips the
	// greeting. The sideband dial above already consumed part of that window,
	// so the delay is anchored at ackedAt rather than now.
	delay := time.Duration(oaiCfg.GreetingDelayMs) * time.Millisecond
	ctrl.GreetAfter(time.Until(ackedAt.Add(delay)))
}

func (s *Server) onAck(req *sip.Request, tx sip.ServerTransaction) {
	if err := s.dlg.ReadAck(req, tx); err != nil {
		s.log.Debug("ReadAck", "callid", req.CallID().Value(), "err", err)
	}
}

func (s *Server) onBye(req *sip.Request, tx sip.ServerTransaction) {
	sipCallID := req.CallID().Value()
	// ReadBye builds and sends the 200 OK itself, so wrap the transaction to
	// stamp the usage header onto it. Look the call up before endCall drops it.
	s.mu.Lock()
	ac := s.calls[sipCallID]
	s.mu.Unlock()
	if h := usageHeaderFor(ac); h != nil {
		tx = &headerTx{ServerTransaction: tx, extra: []sip.Header{h}}
	}
	if err := s.dlg.ReadBye(req, tx); err != nil {
		s.log.Debug("ReadBye", "callid", sipCallID, "err", err)
	}
	s.log.With("callid", sipCallID).Info("call ended (caller BYE)")
	s.endCall(sipCallID, false)
}

// createCallStatus maps a CreateCall failure to the INVITE's final response
// and to the error that goes into the X-Sip2ai-Error header. OpenAI's HTTP
// status is the only signal, and with the per-call api_key override a bad key
// or a throttled account is the failure callers actually hit, so those get
// their own codes instead of masquerading as an SDP problem:
//
//	401/403, caller's key -> 403 Forbidden
//	401/403, server's key -> 502 Bad Gateway (gateway misconfigured, not the caller)
//	429                   -> 480 Temporarily Unavailable (retry later)
//	other 4xx             -> 488 Not Acceptable Here (offer rejected)
//	anything else         -> 502 Bad Gateway
//
// When the server's own key is rejected the detail is the reason alone:
// OpenAI's message echoes a masked form of the key, which is not the
// caller's to see.
func createCallStatus(err error, callerKey bool) (code int, reason string, detail error) {
	var apiErr *openai.APIError
	if !errors.As(err, &apiErr) {
		return sip.StatusBadGateway, "OpenAI error", err
	}
	switch {
	case apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden:
		if callerKey {
			return sip.StatusForbidden, "OpenAI rejected api_key", err
		}
		reason = "OpenAI rejected server credentials"
		return sip.StatusBadGateway, reason, errors.New(reason)
	case apiErr.Status == http.StatusTooManyRequests:
		return sip.StatusTemporarilyUnavailable, "OpenAI rate limited", err
	case apiErr.Status >= 400 && apiErr.Status < 500:
		return sip.StatusNotAcceptableHere, "OpenAI rejected offer", err
	}
	return sip.StatusBadGateway, "OpenAI error", err
}

// errorDetailsJSON serializes err into a compact single-line JSON object for
// the X-Sip2ai-Error response header. A SIP header value cannot contain CRLF,
// so any newlines from the error are stripped; json.Marshal already escapes
// other control characters and quotes. Marshaling cannot realistically fail
// for a plain string, but on the off chance it does we fall back to a literal.
func errorDetailsJSON(err error) string {
	b, mErr := json.Marshal(map[string]string{"error": err.Error()})
	if mErr != nil {
		return `{"error":"serialization failed"}`
	}
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(string(b))
}

// usageHeaderFor builds the X-Sip2ai-Usage header for a call, or nil when there
// is nothing to report (no call, or its sideband control never came up).
func usageHeaderFor(ac *activeCall) sip.Header {
	if ac == nil || ac.ctrl == nil {
		return nil
	}
	return sip.NewHeader(usageHeader, usageHeaderValue(ac.ctrl.Usage()))
}

// usageHeaderValue serializes token totals into a compact single-line JSON
// object. The value is all integers, so it needs no CRLF scrubbing and cannot
// realistically fail to marshal; an empty object is the fallback if it does.
func usageHeaderValue(u openai.Usage) string {
	b, err := json.Marshal(u)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// headerTx wraps a ServerTransaction so that extra headers are appended to
// every response it sends. It lets us add headers to responses sipgo builds
// internally (ReadBye's 200 OK), which we never get our hands on otherwise.
type headerTx struct {
	sip.ServerTransaction
	extra []sip.Header
}

func (t *headerTx) Respond(res *sip.Response) error {
	for _, h := range t.extra {
		res.AppendHeader(h)
	}
	return t.ServerTransaction.Respond(res)
}

// parseConfigHeader extracts and JSON-parses the X-Sip2ai-Config header.
// Returns (nil, nil) when the header is absent.
func parseConfigHeader(req *sip.Request) (*config.CallOverride, error) {
	h := req.GetHeader(configHeader)
	if h == nil {
		return nil, nil
	}
	var override config.CallOverride
	if err := json.Unmarshal([]byte(h.Value()), &override); err != nil {
		return nil, fmt.Errorf("parse %s: %w", configHeader, err)
	}
	if err := override.Validate(); err != nil {
		return nil, fmt.Errorf("invalid %s: %w", configHeader, err)
	}
	return &override, nil
}

// controlOpts builds the sideband session config from the effective per-call
// OpenAI config and transfer destinations.
func (s *Server) controlOpts(oaiCfg config.OpenAIConfig, transfers map[string]string) openai.ControlOptions {
	return openai.ControlOptions{
		Voice:        oaiCfg.Voice,
		Instructions: oaiCfg.SystemPrompt,
		Greeting:     oaiCfg.Greeting,
		HangupDesc:   oaiCfg.HangupToolDesc,
		TransferDesc: oaiCfg.TransferToolDesc,
		Transfers:    transfers,
	}
}

// endCall tears down a call exactly once. sendBye=true sends a SIP BYE to the
// caller (model-initiated hangup); false when the caller already sent BYE.
func (s *Server) endCall(sipCallID string, sendBye bool) {
	s.mu.Lock()
	ac := s.calls[sipCallID]
	delete(s.calls, sipCallID)
	s.mu.Unlock()
	if ac == nil {
		return // already torn down
	}
	log := s.log.With("callid", sipCallID)

	if sendBye && ac.dlg != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := sendByeWithHeaders(ctx, ac.dlg, usageHeaderFor(ac)); err != nil {
			log.Warn("send BYE failed", "err", err)
		}
		cancel()
	}
	if ac.ctrl != nil {
		_ = ac.ctrl.Close()
	}
	hangupOpenAI(ac.oai, ac.oaiCallID, log)
}

// hangupOpenAI ends the OpenAI leg via oai, which must be the client the call
// was created with so the hangup carries the same api_key. An empty callID
// (the call never reached OpenAI) is a no-op.
func hangupOpenAI(oai *openai.Client, callID string, log *slog.Logger) {
	if callID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := oai.Hangup(ctx, callID); err != nil {
		log.Warn("OpenAI hangup failed", "openai_call_id", callID, "err", err)
	}
}

// sendByeWithHeaders sends the in-dialog BYE with extra headers attached. It
// mirrors sipgo's DialogServerSession.Bye (request-URI = the caller's Contact,
// same transport), which takes no headers of its own. nil headers are skipped;
// if the caller's INVITE had no Contact we fall back to sipgo's own Bye.
func sendByeWithHeaders(ctx context.Context, dlg *sipgo.DialogServerSession, extra ...sip.Header) error {
	inv := dlg.InviteRequest
	cont := inv.Contact()
	if cont == nil {
		return dlg.Bye(ctx)
	}
	bye := sip.NewRequest(sip.BYE, cont.Address)
	bye.SetTransport(inv.Transport())
	for _, h := range extra {
		if h != nil {
			bye.AppendHeader(h)
		}
	}
	return dlg.WriteBye(ctx, bye)
}
