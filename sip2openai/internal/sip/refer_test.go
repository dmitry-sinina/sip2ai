package sip

import (
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestBuildReferRequest(t *testing.T) {
	recipient := sip.Uri{User: "alice", Host: "192.0.2.10", Port: 5060}

	tests := []struct {
		name       string
		dest       string
		wantRefer  string
		referredBy string
	}{
		{"tel URI", "tel:42", "<tel:42>", "sip:sip2openai@gw.example.com:5060"},
		{"sip URI", "sip:sales@example.com", "<sip:sales@example.com>", "sip:sip2openai@gw.example.com:5060"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := buildReferRequest(recipient, tc.dest, tc.referredBy)

			if req.Method != sip.REFER {
				t.Errorf("method = %v, want REFER", req.Method)
			}
			if got := req.Recipient.String(); got != recipient.String() {
				t.Errorf("recipient = %q, want %q", got, recipient.String())
			}
			rt := req.GetHeader("Refer-To")
			if rt == nil || rt.Value() != tc.wantRefer {
				t.Errorf("Refer-To = %v, want %q", rt, tc.wantRefer)
			}
			rb := req.GetHeader("Referred-By")
			if rb == nil || rb.Value() != "<"+tc.referredBy+">" {
				t.Errorf("Referred-By = %v, want <%s>", rb, tc.referredBy)
			}
		})
	}
}

func TestSipfragStatus(t *testing.T) {
	tests := []struct {
		frag string
		want int
	}{
		{"SIP/2.0 100 Trying", 100},
		{"SIP/2.0 200 OK", 200},
		{"SIP/2.0 486 Busy Here", 486},
		{"  SIP/2.0 180 Ringing\r\n", 180},
		{"garbage", 0},
		{"", 0},
		{"SIP/2.0 notanumber", 0},
	}
	for _, tc := range tests {
		if got := sipfragStatus(tc.frag); got != tc.want {
			t.Errorf("sipfragStatus(%q) = %d, want %d", tc.frag, got, tc.want)
		}
	}
}
