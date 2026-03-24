# sip2ai

Go SIP-to-AI voice bridge. Accepts inbound SIP calls from a PBX (Asterisk / FreeSWITCH) and bridges the audio to an AI voice backend in real-time.

**Supported AI providers**

| Provider | Protocol | Audio format |
|---|---|---|
| OpenAI Realtime | WebSocket | G.711 µ-law 8 kHz (native) |
| Deepgram Voice Agent | WebSocket | G.711 µ-law 8 kHz (native) |
| Gemini Live (BidiGenerateContent) | WebSocket | PCM16 16 kHz send / 24 kHz recv |

---

## Requirements

- Go 1.23+
- A SIP PBX that can route calls to this process (Asterisk, FreeSWITCH, Kamailio, …)
- API key for at least one AI provider

---

## Build

```bash
git clone <repo>
cd sip2ai
go build -o sip2ai ./cmd/sip2ai
```

Or run directly without a binary:

```bash
go run ./cmd/sip2ai
```

