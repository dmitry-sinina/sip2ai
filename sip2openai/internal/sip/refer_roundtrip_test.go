package sip

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// bindUDP binds a UDP socket synchronously (so there is no listen race) and
// returns it with the chosen port. Serving is started later, via serve, after
// handlers are registered — registering handlers while ServeUDP runs races on
// sipgo's internal handler map.
func bindUDP(t *testing.T) (net.PacketConn, int) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	return pc, pc.LocalAddr().(*net.UDPAddr).Port
}

func serve(srv *sipgo.Server, pc net.PacketConn) {
	go func() { _ = srv.ServeUDP(pc) }()
}

// TestReferRoundTrip drives a real INVITE/200/ACK handshake between two sipgo
// UAs, then has the UAS (our sendRefer) send an in-dialog REFER to the UAC and
// asserts the wire-level Refer-To / Referred-By headers.
func TestReferRoundTrip(t *testing.T) {
	sip.SetDefaultLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	// --- UAC (caller): sends INVITE, must also serve to receive the REFER. ---
	uacUA, _ := sipgo.NewUA(sipgo.WithUserAgent("uac"))
	uacCli, _ := sipgo.NewClient(uacUA)
	uacSrv, _ := sipgo.NewServer(uacUA)
	uacPC, uacPort := bindUDP(t)
	uacContact := sip.ContactHeader{Address: sip.Uri{User: "caller", Host: "127.0.0.1", Port: uacPort}}
	dcc := sipgo.NewDialogClientCache(uacCli, uacContact)

	referCh := make(chan *sip.Request, 1)
	uacSrv.OnRefer(func(req *sip.Request, tx sip.ServerTransaction) {
		referCh <- req
		_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusAccepted, "Accepted", nil))
	})
	serve(uacSrv, uacPC) // handlers registered; safe to serve now

	// --- UAS (our side): answers the INVITE, then sends the REFER. ---
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
	serve(uasSrv, uasPC) // handlers registered; safe to serve now

	srvUnderTest := &Server{contact: uasContact, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// UAC places the call.
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

	// The thing under test: send the REFER.
	if err := srvUnderTest.sendRefer(ctx, uasDlg, "tel:42"); err != nil {
		t.Fatalf("sendRefer: %v", err)
	}

	select {
	case refer := <-referCh:
		if refer.Method != sip.REFER {
			t.Errorf("method = %v, want REFER", refer.Method)
		}
		if rt := refer.GetHeader("Refer-To"); rt == nil || rt.Value() != "<tel:42>" {
			t.Errorf("Refer-To = %v, want <tel:42>", rt)
		}
		if rb := refer.GetHeader("Referred-By"); rb == nil || rb.Value() == "" {
			t.Errorf("Referred-By missing: %v", rb)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("UAC never received REFER")
	}
}
