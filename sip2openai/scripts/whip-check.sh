#!/usr/bin/env bash
#
# whip-check.sh — sip2openai stage-0 (M0) validation gate.
#
# POSTs a SIP-originated WebRTC SDP offer to OpenAI's Realtime "calls" (WHIP)
# endpoint exactly the way sip2openai will, and reports the four decisive facts:
#
#   1. HTTP status      -> is the offer accepted at all? (201 = green)
#   2. Location/call_id  -> the handle for the sideband control WebSocket
#   3. negotiated codec  -> which payload OpenAI picked (confirms no transcode)
#   4. answer size       -> 200-OK-over-UDP budget vs sipgo's 1300-byte cap
#
# It then best-effort hangs up the test call so nothing is left dangling.
#
# Usage:
#   export OPENAI_API_KEY=sk-...
#   ./whip-check.sh [offer.sdp]
#
# Flags (env):
#   MODEL=gpt-realtime     # model query param
#   INJECT_MID=1           # add a=mid:0 + a=group:BUNDLE 0 if absent (test #2)
#   KEEP=1                 # do NOT hang up the call afterwards
#
set -euo pipefail

# --- locate offer ----------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OFFER="${1:-$SCRIPT_DIR/offer.sdp}"
MODEL="${MODEL:-gpt-realtime}"

# --- colors ----------------------------------------------------------------
if [[ -t 1 ]]; then
  R=$'\e[31m'; G=$'\e[32m'; Y=$'\e[33m'; B=$'\e[34m'; BOLD=$'\e[1m'; N=$'\e[0m'
else
  R=''; G=''; Y=''; B=''; BOLD=''; N=''
fi
say()  { printf '%s\n' "$*"; }
ok()   { printf '%s✓%s %s\n' "$G" "$N" "$*"; }
warn() { printf '%s!%s %s\n' "$Y" "$N" "$*"; }
err()  { printf '%s✗%s %s\n' "$R" "$N" "$*"; }
hdr()  { printf '\n%s== %s ==%s\n' "$BOLD" "$*" "$N"; }

# --- preflight -------------------------------------------------------------
[[ -n "${OPENAI_API_KEY:-}" ]] || { err "OPENAI_API_KEY is not set"; exit 2; }
[[ -f "$OFFER" ]] || { err "offer file not found: $OFFER"; exit 2; }

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
SEND="$WORK/send.sdp"
cp "$OFFER" "$SEND"

# --- optional mid/BUNDLE injection (risk #2 from the analysis) --------------
if [[ "${INJECT_MID:-0}" == "1" ]]; then
  if grep -qi '^a=mid:' "$SEND"; then
    warn "offer already has a=mid — skipping injection"
  else
    # add group:BUNDLE 0 right after the session c= line, and mid:0 after m=audio
    awk '
      /^m=audio/ { print; print "a=mid:0"; injected=1; next }
      { print }
      END { }
    ' "$SEND" > "$SEND.tmp" && mv "$SEND.tmp" "$SEND"
    # group line belongs in the session section, before the first m=
    awk '
      /^m=/ && !done { print "a=group:BUNDLE 0"; done=1 }
      { print }
    ' "$SEND" > "$SEND.tmp" && mv "$SEND.tmp" "$SEND"
    ok "injected a=group:BUNDLE 0 + a=mid:0"
  fi
fi

OFFER_BYTES="$(wc -c < "$SEND" | tr -d ' ')"

hdr "REQUEST"
say "endpoint : ${B}https://api.openai.com/v1/realtime/calls?model=$MODEL${N}"
say "offer    : $OFFER  (${OFFER_BYTES} bytes)"
say "codecs   : $(grep -i '^a=rtpmap:' "$SEND" | sed 's/^a=rtpmap:/         /' | paste -sd',' - | sed 's/  */ /g')"
grep -qi 'webrtc-datachannel' "$SEND" && say "datachan : ${G}present${N}" || say "datachan : ${Y}absent (audio-only)${N}"
grep -qi '^a=mid:'            "$SEND" && say "mid      : ${G}present${N}" || say "mid      : ${Y}absent${N}"

# --- POST ------------------------------------------------------------------
HDRS="$WORK/resp.headers"
BODY="$WORK/answer.sdp"
STATUS="$(curl -sS -o "$BODY" -D "$HDRS" -w '%{http_code}' \
  -X POST \
  -H "Authorization: Bearer $OPENAI_API_KEY" \
  -H "Content-Type: application/sdp" \
  --data-binary @"$SEND" \
  "https://api.openai.com/v1/realtime/calls?model=$MODEL")" || {
    err "curl failed to reach OpenAI"; exit 1; }

hdr "RESPONSE"
# 1. status
if [[ "$STATUS" == "201" || "$STATUS" == "200" ]]; then
  ok "HTTP $STATUS — offer ACCEPTED"
else
  err "HTTP $STATUS — offer REJECTED"
fi

# 2. Location / call_id
LOCATION="$(grep -i '^location:' "$HDRS" | sed 's/^[Ll]ocation:[[:space:]]*//; s/\r$//' | tail -1)"
CALL_ID=""
if [[ -n "$LOCATION" ]]; then
  CALL_ID="$(basename "$LOCATION" | sed 's/\r$//')"
  ok "call_id  : ${BOLD}$CALL_ID${N}   (Location: $LOCATION)"
  say "  sideband control WS -> wss://api.openai.com/v1/realtime?call_id=$CALL_ID"
else
  warn "no Location header (no call_id) — server-side control would be unavailable"
fi

# 3 + 4 only meaningful on success
if [[ "$STATUS" == "201" || "$STATUS" == "200" ]]; then
  ANS_BYTES="$(wc -c < "$BODY" | tr -d ' ')"
  MLINE="$(grep -i '^m=audio' "$BODY" | head -1)"
  CHOSEN_PT="$(awk '/^m=audio/{print $4; exit}' "$BODY")"
  CHOSEN_CODEC="$(grep -i "^a=rtpmap:$CHOSEN_PT " "$BODY" | sed 's/^a=rtpmap:[0-9]* //; s/\r$//')"
  say ""
  ok "negotiated codec : ${BOLD}${CHOSEN_CODEC:-?}${N}  (payload $CHOSEN_PT)"
  say "  m-line: $MLINE"
  grep -qi 'webrtc-datachannel' "$BODY" && say "  answer includes a data channel" || true

  # 4. size budget vs sipgo UDP cap (1500-200=1300)
  if (( ANS_BYTES > 1300 )); then
    warn "answer is ${BOLD}${ANS_BYTES}${N} bytes — EXCEEDS sipgo's 1300 B UDP cap"
    say  "  -> 200 OK over UDP will fail with ErrUDPMTUCongestion; plan MTU mitigation"
  else
    ok "answer size : ${ANS_BYTES} bytes — within the 1300 B UDP budget"
  fi
else
  hdr "ERROR BODY"
  cat "$BODY"; echo
fi

hdr "ANSWER SDP"
cat "$BODY"; echo

# --- cleanup: hang up the test call ----------------------------------------
if [[ -n "$CALL_ID" && "${KEEP:-0}" != "1" ]]; then
  hdr "CLEANUP"
  HUP="$(curl -sS -o /dev/null -w '%{http_code}' \
    -X POST \
    -H "Authorization: Bearer $OPENAI_API_KEY" \
    "https://api.openai.com/v1/realtime/calls/$CALL_ID/hangup" 2>/dev/null || echo "000")"
  if [[ "$HUP" == "200" || "$HUP" == "204" ]]; then
    ok "hung up test call $CALL_ID (HTTP $HUP)"
  else
    warn "hangup returned HTTP $HUP — call will time out on its own (no media ever connects)"
  fi
fi

say ""
say "${BOLD}Next:${N} paste the ANSWER SDP back so we can lock M1 (codec / size / mid)."
