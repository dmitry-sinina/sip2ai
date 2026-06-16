// Package sip is the SIP UAS for sip2openai. It terminates signaling only:
// inbound INVITE offers are relayed to OpenAI and the answer is returned, while
// media (ICE/DTLS-SRTP/Opus or G.711) flows directly between caller and OpenAI.
// A per-call sideband WebSocket carries session config and tool calls.
package sip

import (
	"context"
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

	ua  *sipgo.UserAgent
	srv *sipgo.Server
	cli *sipgo.Client
	dlg *sipgo.DialogServerCache

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
		calls:     make(map[string]*activeCall),
	}
	srv.OnInvite(s.onInvite)
	srv.OnAck(s.onAck)
	srv.OnBye(s.onBye)
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

	dlg, err := s.dlg.ReadInvite(req, tx)
	if err != nil {
		log.Error("ReadInvite failed", "err", err)
		return
	}
	_ = dlg.Respond(sip.StatusTrying, "Trying", nil)

	// Make the telephony offer JSEP-idiomatic, then relay to OpenAI.
	normOffer := sdpx.EnsureBundleMid(offer)
	answer, oaiCallID, err := s.oai.CreateCall(dlg.Context(), normOffer)
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
	ctrl := s.oai.NewControl(oaiCallID, s.controlOpts(), s.oaiLog.With("callid", sipCallID))
	ctrl.OnHangup = func() { s.endCall(sipCallID, true) }
	ctrl.OnTransfer = func(uri string) {
		log.Warn("transfer requested — SIP REFER not implemented yet (M3)", "uri", uri)
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

// controlOpts builds the per-call sideband session config from server config.
func (s *Server) controlOpts() openai.ControlOptions {
	return openai.ControlOptions{
		Voice:        s.oaiCfg.Voice,
		Instructions: s.oaiCfg.SystemPrompt,
		Greeting:     s.oaiCfg.Greeting,
		HangupDesc:   s.oaiCfg.HangupToolDesc,
		TransferDesc: s.oaiCfg.TransferToolDesc,
		Transfers:    s.transfers,
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
