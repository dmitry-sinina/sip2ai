// Package sdp performs the minimal, structural-only SDP rewrites that let a
// telephony-style WebRTC offer (e.g. from SEMS) interoperate with OpenAI's
// Realtime WebRTC ("calls"/WHIP) endpoint, without ever touching ICE, DTLS,
// codec or candidate lines — so media stays a direct, untranscoded relay.
package sdp

import (
	"strconv"
	"strings"
)

func detectNL(s string) string {
	if strings.Contains(s, "\r\n") {
		return "\r\n"
	}
	return "\n"
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// EnsureBundleMid makes an offer JSEP/WebRTC-idiomatic so OpenAI accepts it:
// every m= section gains an a=mid (if missing) and a session-level
// a=group:BUNDLE lists all mids. Only mid/group lines are added; everything
// else is copied verbatim. Idempotent: existing mids and an existing BUNDLE
// group are preserved.
func EnsureBundleMid(in []byte) []byte {
	nl := detectNL(string(in))
	lines := splitLines(string(in))

	// Pass 1: resolve each section's mid (existing or to-assign) + detect group.
	var mids []string
	var assigned []bool
	hasGroup := false
	sect := -1
	for _, ln := range lines {
		switch {
		case strings.HasPrefix(ln, "a=group:BUNDLE"):
			hasGroup = true
		case strings.HasPrefix(ln, "m="):
			sect++
			mids = append(mids, "")
			assigned = append(assigned, false)
		case strings.HasPrefix(ln, "a=mid:") && sect >= 0 && mids[sect] == "":
			mids[sect] = strings.TrimSpace(strings.TrimPrefix(ln, "a=mid:"))
		}
	}
	for i := range mids {
		if mids[i] == "" {
			mids[i] = strconv.Itoa(i)
			assigned[i] = true
		}
	}

	// Pass 2: rebuild, inserting group before the first m= and mids after each
	// m= that lacked one.
	out := make([]string, 0, len(lines)+len(mids)+1)
	sect = -1
	groupInserted := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, "m=") {
			if !hasGroup && !groupInserted && len(mids) > 0 {
				out = append(out, "a=group:BUNDLE "+strings.Join(mids, " "))
				groupInserted = true
			}
			sect++
			out = append(out, ln)
			if assigned[sect] {
				out = append(out, "a=mid:"+mids[sect])
			}
			continue
		}
		out = append(out, ln)
	}
	return []byte(strings.Join(out, nl) + nl)
}

// StripBundleMid removes the a=group:BUNDLE and a=mid lines, restoring symmetry
// for a telephony UA that never offered them. Applied to OpenAI's answer before
// it is relayed back in the SIP 200 OK.
func StripBundleMid(in []byte) []byte {
	nl := detectNL(string(in))
	lines := splitLines(string(in))
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if strings.HasPrefix(ln, "a=mid:") || strings.HasPrefix(ln, "a=group:BUNDLE") {
			continue
		}
		out = append(out, ln)
	}
	return []byte(strings.Join(out, nl) + nl)
}
