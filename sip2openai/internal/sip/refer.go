package sip

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// sendRefer performs a blind (unattended) transfer by sending an in-dialog
// REFER to the caller, asking it to contact destURI. sipgo's DialogServerSession
// fills the in-dialog routing (To/From/Call-ID/CSeq/Route/Contact) via Do; we
// only add Refer-To and Referred-By. A 202 Accepted means the caller took the
// referral — it then sets up the new call itself and tears our leg down.
func (s *Server) sendRefer(ctx context.Context, dlg *sipgo.DialogServerSession, destURI string) error {
	if st := dlg.LoadState(); st != sip.DialogStateConfirmed {
		return fmt.Errorf("dialog not confirmed (state=%v), cannot REFER", st)
	}
	cont := dlg.InviteRequest.Contact()
	if cont == nil {
		return fmt.Errorf("caller INVITE has no Contact header")
	}
	refer := buildReferRequest(cont.Address, destURI, s.contact.Address.String())
	refer.SetTransport(dlg.InviteRequest.Transport())

	res, err := dlg.Do(ctx, refer)
	if err != nil {
		return fmt.Errorf("send REFER: %w", err)
	}
	if res.StatusCode != sip.StatusAccepted {
		return fmt.Errorf("REFER not accepted: %d %s", res.StatusCode, res.Reason)
	}
	return nil
}

// buildReferRequest constructs a REFER targeting recipient (the caller's
// Contact), referring it to destURI on behalf of referredBy. destURI/referredBy
// are wrapped in angle brackets as name-addr; destURI is used verbatim so tel:
// URIs (e.g. "tel:42") survive without going through sipgo's URI parser.
func buildReferRequest(recipient sip.Uri, destURI, referredBy string) *sip.Request {
	refer := sip.NewRequest(sip.REFER, recipient)
	refer.AppendHeader(sip.NewHeader("Refer-To", "<"+destURI+">"))
	refer.AppendHeader(sip.NewHeader("Referred-By", "<"+referredBy+">"))
	return refer
}

// onRefer rejects inbound REFER: sip2openai initiates transfers but does not
// accept them (no UA on our side to be redirected).
func (s *Server) onRefer(req *sip.Request, tx sip.ServerTransaction) {
	_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotImplemented, "Not Implemented", nil))
}

// onNotify consumes in-dialog NOTIFYs. For REFER progress (message/sipfrag) it
// acks with 200 and, on a final response (status >= 200), tears down our leg —
// mirroring the common UAC behavior after a completed blind transfer.
func (s *Server) onNotify(req *sip.Request, tx sip.ServerTransaction) {
	sipCallID := req.CallID().Value()
	log := s.log.With("callid", sipCallID)

	_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))

	ct := req.ContentType()
	if ct == nil || !strings.HasPrefix(ct.Value(), "message/sipfrag") {
		return // not a REFER progress NOTIFY
	}
	frag := strings.TrimSpace(string(req.Body()))
	code := sipfragStatus(frag)
	log.Info("REFER progress NOTIFY", "sipfrag", frag, "status", code)
	if code >= 200 {
		log.Info("transfer complete, tearing down leg", "status", code)
		s.endCall(sipCallID, true)
	}
}

// sipfragStatus extracts the status code from a message/sipfrag status line
// such as "SIP/2.0 200 OK". Returns 0 if it cannot be parsed.
func sipfragStatus(frag string) int {
	fields := strings.Fields(frag)
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "SIP/2.0") {
		return 0
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return code
}
