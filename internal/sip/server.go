package sip

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/emiago/diago"
	diagomedia "github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	sipsip "github.com/emiago/sipgo/sip"
	"sip2ai/internal/ai"
	"sip2ai/internal/config"
	rtpsession "sip2ai/internal/rtp"
)

const configHeader = "X-Sip2ai-Config"

// Server is a SIP User Agent Server that handles inbound calls and bridges
// them to an AI voice provider.
type Server struct {
	diago     *diago.Diago
	aiFactory func(cid string, cfg *config.Config) ai.AIProvider
	logger    *slog.Logger
	cfg       *config.Config

	mu    sync.Mutex
	calls map[string]context.CancelFunc // call_id -> cancel
	wg    sync.WaitGroup
}

// NewServer constructs and configures a Server but does not start it.
func NewServer(cfg *config.Config, aiFactory func(cid string, cfg *config.Config) ai.AIProvider, logger *slog.Logger) (*Server, error) {
	ua, err := sipgo.NewUA(sipgo.WithUserAgent("sip2ai"))
	if err != nil {
		return nil, fmt.Errorf("sipgo UA: %w", err)
	}

	transport := diago.Transport{
		Transport:    cfg.SIP.Transport,
		BindHost:     cfg.SIP.BindHost,
		BindPort:     cfg.SIP.BindPort,
		ExternalHost: cfg.SIP.ExternalHost,
		ExternalPort: cfg.SIP.ExternalPort,
	}
	if cfg.SIP.MediaExternalIP != "" {
		transport.MediaExternalIP = net.ParseIP(cfg.SIP.MediaExternalIP)
	}

	// Only advertise PCMU - all AI providers are configured for mulaw 8 kHz.
	mediaConf := diago.MediaConfig{
		Codecs: []diagomedia.Codec{diagomedia.CodecAudioUlaw},
	}

	d := diago.NewDiago(ua,
		diago.WithTransport(transport),
		diago.WithMediaConfig(mediaConf),
		diago.WithLogger(logger),
	)

	return &Server{
		diago:     d,
		aiFactory: aiFactory,
		logger:    logger,
		cfg:       cfg,
		calls:     make(map[string]context.CancelFunc),
	}, nil
}

// Start begins serving inbound SIP calls. Blocks until ctx is cancelled.
// On shutdown: stops accepting new calls, terminates all active calls with
// BYE, and waits for them to finish cleanly.
func (s *Server) Start(ctx context.Context) error {
	s.logger.Info("SIP server starting",
		"host", s.cfg.SIP.BindHost,
		"port", s.cfg.SIP.BindPort,
		"transport", s.cfg.SIP.Transport,
	)

	err := s.diago.Serve(ctx, func(d *diago.DialogServerSession) {
		s.handleCall(d)
	})

	// ctx cancelled - diago.Serve returned. Now terminate all active calls.
	s.logger.Info("shutting down, terminating active calls")
	s.mu.Lock()
	for id, cancel := range s.calls {
		s.logger.Info("terminating call", "call_id", id)
		cancel()
	}
	s.mu.Unlock()

	// Wait for all handleCall goroutines to finish (hangup + cleanup).
	s.wg.Wait()
	s.logger.Info("all calls terminated")

	return err
}

func (s *Server) handleCall(dialog *diago.DialogServerSession) {
	cid := ""
	if h := dialog.InviteRequest.CallID(); h != nil {
		cid = h.Value()
	}
	log := s.logger.With("cid", cid)
	log.Info("incoming call")

	// Parse per-call config override from X-Sip2ai-Config header.
	callCfg := s.cfg
	if override, err := parseConfigHeader(dialog.InviteRequest); err != nil {
		log.Warn("bad X-Sip2ai-Config header, using defaults", "err", err)
	} else if override != nil {
		callCfg = s.cfg.WithOverride(override)
		log.Info("per-call config override applied", "provider", callCfg.AI.Provider)
	}

	if err := dialog.Ringing(); err != nil {
		log.Error("ringing failed", "err", err)
		return
	}

	callCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Track this call for graceful shutdown.
	s.mu.Lock()
	s.calls[cid] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	defer func() {
		s.mu.Lock()
		delete(s.calls, cid)
		s.mu.Unlock()
		s.wg.Done()
	}()

	provider := s.aiFactory(cid, callCfg)
	if err := rtpsession.ConnectWithRetry(callCtx, provider, callCfg.AI, log); err != nil {
		log.Error("AI connect failed, rejecting call", "err", err)
		dialog.Respond(sipsip.StatusServiceUnavailable, "Service Unavailable", nil) //nolint:errcheck
		return
	}

	if err := dialog.Answer(); err != nil {
		log.Error("answer failed", "err", err)
		return
	}
	defer func() {
		hangCtx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer hcancel()
		dialog.Hangup(hangCtx) //nolint:errcheck
		log.Info("call ended")
	}()

	audioReader, err := dialog.AudioReader()
	if err != nil {
		log.Error("audio reader failed", "err", err)
		return
	}

	audioWriter, err := dialog.AudioWriter()
	if err != nil {
		log.Error("audio writer failed", "err", err)
		return
	}

	sess := rtpsession.NewCallSession(
		callCtx,
		cancel,
		audioReader,
		audioWriter,
		provider,
		log,
		callCfg.AI,
	)
	transfer := sess.Run()

	if transfer != nil {
		var referTo sipsip.Uri
		if err := sipsip.ParseUri(transfer.Destination, &referTo); err != nil {
			log.Error("SIP REFER: invalid URI", "err", err, "destination", transfer.Destination)
		} else {
			referCtx, referCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer referCancel()
			if err := dialog.Refer(referCtx, referTo); err != nil {
				log.Error("SIP REFER failed", "err", err, "destination", transfer.Destination)
			} else {
				log.Info("SIP REFER sent", "destination", transfer.Destination)
			}
		}
	}
}

// parseConfigHeader extracts and parses the X-Sip2ai-Config JSON header.
// Returns (nil, nil) if the header is absent.
func parseConfigHeader(req *sipsip.Request) (*config.CallOverride, error) {
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
