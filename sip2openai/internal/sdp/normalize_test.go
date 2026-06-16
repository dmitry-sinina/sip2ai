package sdp

import (
	"strings"
	"testing"
)

// Real SEMS offer captured from the client (trimmed candidate set).
const semsOffer = `v=0
o=sems 52169153 829957026 IN IP4 46.19.213.104
s=sems
c=IN IP4 46.19.213.104
t=0 0
m=audio 30400 UDP/TLS/RTP/SAVPF 9 0 8 96
a=rtpmap:9 g722/8000
a=rtpmap:0 PCMU/8000
a=rtpmap:8 PCMA/8000
a=rtpmap:96 telephone-event/8000
a=fingerprint:SHA-256 82:43:12:9F:B9:5E
a=ice-pwd:X0zPIZF0Tr294yWsX99q3s
a=ice-ufrag:f8OZ
a=candidate:99160495 1 UDP 2116422143 46.19.213.104 30400 typ host
a=rtcp-mux
a=ptime:20
a=sendrecv
a=setup:actpass
a=ssrc:1533634887 cname:46.19.213.104
`

func lines(s string) []string {
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}

func TestEnsureBundleMid(t *testing.T) {
	out := string(EnsureBundleMid([]byte(semsOffer)))

	if !strings.Contains(out, "a=group:BUNDLE 0") {
		t.Fatalf("missing BUNDLE group:\n%s", out)
	}

	// a=mid:0 must sit directly after the m=audio line.
	ls := lines(out)
	ok := false
	for i, ln := range ls {
		if strings.HasPrefix(ln, "m=audio") && i+1 < len(ls) && ls[i+1] == "a=mid:0" {
			ok = true
		}
	}
	if !ok {
		t.Errorf("a=mid:0 not directly after m=audio:\n%s", out)
	}

	// group must appear before the m= line (session-level).
	if gi, mi := strings.Index(out, "a=group:BUNDLE"), strings.Index(out, "m=audio"); gi < 0 || gi > mi {
		t.Errorf("group not before m=: gi=%d mi=%d", gi, mi)
	}

	// ICE / DTLS / codec lines must be untouched.
	for _, must := range []string{
		"a=fingerprint:SHA-256 82:43:12:9F:B9:5E",
		"a=ice-ufrag:f8OZ",
		"a=candidate:99160495 1 UDP 2116422143 46.19.213.104 30400 typ host",
		"a=setup:actpass",
		"a=rtpmap:0 PCMU/8000",
	} {
		if !strings.Contains(out, must) {
			t.Errorf("offer mangled, missing %q", must)
		}
	}
}

func TestEnsureBundleMidIdempotent(t *testing.T) {
	once := EnsureBundleMid([]byte(semsOffer))
	twice := EnsureBundleMid(once)
	if string(once) != string(twice) {
		t.Errorf("not idempotent:\n--once--\n%s\n--twice--\n%s", once, twice)
	}
}

func TestStripBundleMid(t *testing.T) {
	in := EnsureBundleMid([]byte(semsOffer))
	out := string(StripBundleMid(in))
	if strings.Contains(out, "a=mid:") || strings.Contains(out, "a=group:BUNDLE") {
		t.Errorf("strip left mid/group behind:\n%s", out)
	}
	if !strings.Contains(out, "a=rtpmap:0 PCMU/8000") || !strings.Contains(out, "a=ice-ufrag:f8OZ") {
		t.Errorf("strip removed too much:\n%s", out)
	}
}
