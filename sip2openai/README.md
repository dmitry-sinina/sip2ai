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
  → 200 OK (answer)  ← ACK
  ⇒ media: caller ⇄ OpenAI, direct (proxy absent)
  → sideband WS: session.update   (prompt, voice, tools)
  → greeting, greeting_delay_ms after the ACK (ICE/DTLS ran meanwhile)
BYE → POST /v1/realtime/calls/{call_id}/hangup
```

## Status — Stage 1 (M1 + M2 done)

Implemented:
- SIP UAS over **UDP + TCP** (sipgo), INVITE / ACK / BYE.
- SDP normalization: `EnsureBundleMid` (offer → OpenAI) / `StripBundleMid` (answer → caller).
- OpenAI calls client: `CreateCall` (offer→answer+call_id), `Hangup`.
- `udp_mtu` knob to clear sipgo's 1300 B UDP cap for large WebRTC answers.
- **Sideband control WebSocket** per call: session config (system prompt + voice),
  greeting (held back past ICE/DTLS setup, see below), token accounting,
  keepalive (idle-drop mitigation).
- **Token usage reporting** to the caller via the `X-Sip2ai-Usage` header on BYE.
- **`hangup_call`** tool → SIP BYE to the caller.

Not yet (next milestones):
- **M3** — `transfer_call` → SIP REFER; CANCEL mapping; richer error→status mapping.
- **M4** — hardening: sideband reconnect, Prometheus metrics, packaging (Dockerfile/Helm).

> `transfer_call` is surfaced over the sideband but currently only logged — the
> REFER mapping lands in M3.

## SIP headers

| Header | Direction | Purpose |
| --- | --- | --- |
| `X-Sip2ai-Config` | INVITE (in) | Per-call JSON config override (`api_key`, `model`, `voice`, `prompt`, `greeting`, `greeting_delay_ms`, `hangup_tool_desc`, `transfer_tool_desc`, `transfers`). Malformed JSON, an empty `api_key` or a negative `greeting_delay_ms` is rejected with `400`. |
| `X-Sip2ai-Error` | response (out) | JSON error details on a rejection, e.g. `{"error":"parse X-Sip2ai-Config: ..."}`. |
| `X-Sip2ai-Usage` | BYE (out) / 200 OK to BYE (out) | The call's token totals. |

An `api_key` in `X-Sip2ai-Config` replaces the server's OpenAI key for that
call only: the SDP offer, the sideband control WebSocket and the final hangup
all authenticate with it, so usage is billed to the caller's own OpenAI
account. The server key (`openai.api_key` / `OPENAI_API_KEY`) is still
required at startup and serves calls that do not override it. Surrounding
whitespace is trimmed; a blank key is rejected with `400`. If OpenAI rejects
the key (HTTP 401/403) the INVITE fails with `403 Forbidden`, and if the
account is throttled (HTTP 429) with `480 Temporarily Unavailable`;
`X-Sip2ai-Error` carries OpenAI's response either way. Other OpenAI rejections
of the offer map to `488`, and transport or upstream failures to `502`.

**The header is a secret sent in cleartext.** sip2openai listens on UDP and
TCP only (no TLS), so an on-path observer reads every caller's key straight
off the INVITE. Accept the override only from a trusted hop, such as an SBC or
a private network between it and sip2openai. The application log never prints
the key (the override log line only reports whether one was supplied), but
`log.sip: debug` dumps every SIP message verbatim at the transport layer,
`X-Sip2ai-Config` included, so keep that level off wherever callers send keys.

```
X-Sip2ai-Config: {"api_key":"sk-...","voice":"verse","prompt":"You are the front desk."}
```

Token totals are only final when the call ends, so `X-Sip2ai-Usage` cannot ride
on the 200 OK to the INVITE — it goes on teardown instead, covering both hangup
directions: the BYE we send when the model calls `hangup_call` (or after a
completed transfer), and our 200 OK when the caller sends the BYE. The value is
a single-line JSON object using OpenAI's own field names:

```
X-Sip2ai-Usage: {"total_tokens":1234,"input_tokens":900,"output_tokens":334,"input_text_tokens":120,"input_audio_tokens":780,"output_text_tokens":34,"output_audio_tokens":300}
```

The header is omitted when the call has no sideband control (it failed to come
up), and reports zeros if the sideband produced no `response.done` events. The
same totals are also logged at call end (`sideband closed`).

## Greeting timing

sip2openai never sees media come up: after the 200 OK the caller runs ICE and
DTLS directly against OpenAI, and nothing on the signaling path reports when
that finishes. A greeting spoken before it does is lost — the caller hears the
call start mid-sentence. The last signal we do get is the caller's **ACK**
(sipgo retransmits the 200 OK until it arrives), so the greeting is scheduled
`openai.greeting_delay_ms` after the ACK (default `1000`; per-call via
`X-Sip2ai-Config`). The sideband dial and `session.update` run inside that
window, not on top of it. Raise the value if callers still miss the first
words; `0` speaks as soon as the sideband is up. If the caller talks before the
delay is up, the greeting is skipped — server VAD already makes the model
answer them.

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
