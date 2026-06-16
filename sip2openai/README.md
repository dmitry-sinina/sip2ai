# sip2openai

Signaling-only gateway between **SIP** and **OpenAI's Realtime WebRTC API**.

Unlike its sibling `sip2ai` (which terminates RTP and bridges audio over a
WebSocket), sip2openai stays **out of the media path**: it relays the caller's
SDP to OpenAI's Realtime "calls" (WHIP) endpoint and returns the answer, so
media (ICE/DTLS-SRTP, G.711/Opus) flows **directly** between the caller and
OpenAI. Call control runs over OpenAI's sideband WebSocket (keyed by `call_id`).

## How a call flows

```
INVITE (SDP offer)
  → inject a=mid/a=group:BUNDLE   (make telephony SDP JSEP-idiomatic)
  → POST /v1/realtime/calls       (Content-Type: application/sdp)
  ← 201 + SDP answer + call_id
  → strip a=mid/a=group           (restore symmetry for the caller)
  → 200 OK (answer)
  ⇒ media: caller ⇄ OpenAI, direct (proxy absent)
BYE → POST /v1/realtime/calls/{call_id}/hangup
```

## Status — Stage 1 (M1 + M2 done)

Implemented:
- SIP UAS over **UDP + TCP** (sipgo), INVITE / ACK / BYE.
- SDP normalization: `EnsureBundleMid` (offer → OpenAI) / `StripBundleMid` (answer → caller).
- OpenAI calls client: `CreateCall` (offer→answer+call_id), `Hangup`.
- `udp_mtu` knob to clear sipgo's 1300 B UDP cap for large WebRTC answers.
- **Sideband control WebSocket** per call: session config (system prompt + voice),
  greeting, token accounting, keepalive (idle-drop mitigation).
- **`hangup_call`** tool → SIP BYE to the caller.

Not yet (next milestones):
- **M3** — `transfer_call` → SIP REFER; CANCEL mapping; richer error→status mapping.
- **M4** — hardening: sideband reconnect, Prometheus metrics, packaging (Dockerfile/Helm).

> `transfer_call` is surfaced over the sideband but currently only logged — the
> REFER mapping lands in M3.

## Run

```bash
go build -o sip2openai ./cmd/sip2openai
OPENAI_API_KEY=sk-... ./sip2openai -config config.yaml
```

Point a SIP/UDP client with WebRTC media (DTLS-SRTP/ICE) at `bind_host:bind_port`.

## Validate an offer (M0 gate)

`scripts/whip-check.sh` POSTs an SDP offer to OpenAI and reports
accept/reject, `call_id`, negotiated codec, and answer size. See the script
header for usage.
