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

// activeCall holds the per-call resources we must tear down together.
type activeCall struct {
	dlg       *sipgo.DialogServerSession
	ctrl      *openai.Control
	oaiCallID string
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
	oaiCfg, transfers := s.oaiCfg, s.transfers
	if override, err := parseConfigHeader(req); err != nil {
		log.Warn("bad "+configHeader+" header, using server config", "err", err)
	} else if override != nil {
		eff := config.Config{OpenAI: s.oaiCfg, Transfers: s.transfers}.WithOverride(override)
		oaiCfg, transfers = eff.OpenAI, eff.Transfers
		log.Info("per-call config override applied", "model", oaiCfg.Model, "voice", oaiCfg.Voice, "transfer_dests", len(transfers))
	}

	dlg, err := s.dlg.ReadInvite(req, tx)
	if err != nil {
		log.Error("ReadInvite failed", "err", err)
		return
	}
	_ = dlg.Respond(sip.StatusTrying, "Trying", nil)

	// Make the telephony offer JSEP-idiomatic, then relay to OpenAI.
	normOffer := sdpx.EnsureBundleMid(offer)
	answer, oaiCallID, err := s.oai.CreateCall(dlg.Context(), normOffer, oaiCfg.Model)
	if err != nil {
		code, reason := sip.StatusBadGateway, "OpenAI error"
		var apiErr *openai.APIError
		if errors.As(err, &apiErr) && apiErr.Status >= 400 && apiErr.Status < 500 {
			code, reason = sip.StatusNotAcceptableHere, "OpenAI rejected offer"
		}
		log.Error("CreateCall failed", "err", err)
		_ = dlg.Respond(code, reason, nil)
		return
	}
	log.Info("OpenAI call created", "openai_call_id", oaiCallID, "answer_bytes", len(answer))

	// Strip the mid/BUNDLE we injected so the answer stays symmetric with the
	// original offer, then return it in the 200 OK.
	answerForClient := sdpx.StripBundleMid(answer)
	if err := dlg.RespondSDP(answerForClient); err != nil {
		log.Error("send 200 OK failed", "err", err, "bytes", len(answerForClient))
		s.hangupOpenAI(oaiCallID, log)
		return
	}
	log.Info("call answered — media direct to OpenAI", "answer_bytes", len(answerForClient))

	// Register the call, then bring up the sideband control plane.
	ac := &activeCall{dlg: dlg, oaiCallID: oaiCallID}
	ctrl := s.oai.NewControl(oaiCallID, s.controlOpts(oaiCfg, transfers), s.oaiLog.With("callid", sipCallID))
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
	} else {
		log.Info("sideband control up")
	}
}

func (s *Server) onAck(req *sip.Request, tx sip.ServerTransaction) {
	if err := s.dlg.ReadAck(req, tx); err != nil {
		s.log.Debug("ReadAck", "callid", req.CallID().Value(), "err", err)
	}
}

func (s *Server) onBye(req *sip.Request, tx sip.ServerTransaction) {
	sipCallID := req.CallID().Value()
	if err := s.dlg.ReadBye(req, tx); err != nil {
		s.log.Debug("ReadBye", "callid", sipCallID, "err", err)
	}
	s.log.With("callid", sipCallID).Info("call ended (caller BYE)")
	s.endCall(sipCallID, false)
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
		if err := ac.dlg.Bye(ctx); err != nil {
			log.Warn("send BYE failed", "err", err)
		}
		cancel()
	}
	if ac.ctrl != nil {
		_ = ac.ctrl.Close()
	}
	s.hangupOpenAI(ac.oaiCallID, log)
}

func (s *Server) hangupOpenAI(callID string, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.oai.Hangup(ctx, callID); err != nil {
		log.Warn("OpenAI hangup failed", "openai_call_id", callID, "err", err)
	}
}
