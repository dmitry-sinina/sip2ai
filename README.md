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

---

## Kubernetes (Helm)

Install from OCI registry:

```bash
helm install sip2ai oci://ghcr.io/<owner>/charts/sip2ai \
  --version 1.0.0 \
  --set secrets.openai=sk-... \
  --set config.ai.provider=openai
```

Or from local checkout:

```bash
helm install sip2ai deploy/helm/sip2ai \
  --set secrets.openai=sk-...
```

### Using an existing secret

Create a secret with provider API keys:

```bash
kubectl create secret generic sip2ai-keys \
  --from-literal=openai=sk-... \
  --from-literal=deepgram=... \
  --from-literal=gemini=...
```

Then reference it:

```bash
helm install sip2ai deploy/helm/sip2ai \
  --set existingSecret=sip2ai-keys
```

### Override config values

All fields under `config:` in `values.yaml` map directly to `config.yaml`:

```bash
helm install sip2ai deploy/helm/sip2ai \
  --set config.ai.provider=openai \
  --set config.openai.voice=shimmer \
  --set config.log.level=debug \
  --set "config.openai.system_prompt=You are a sales agent."
```

### Uninstall

```bash
helm uninstall sip2ai
```

