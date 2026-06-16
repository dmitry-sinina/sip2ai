package config

import (
	"encoding/json"
	"testing"
)

func TestWithOverrideFromHeader(t *testing.T) {
	// The exact X-SIP2AI-CONFIG payload a caller may send.
	const header = `{"greeting": "Hi. How can I help you?", "hangup_tool_desc": "terminate call when caller ask you to do so", "model": "gpt-realtime-2", "prompt": "you are IVR", "provider": "openai", "transfer_tool_desc": "transfer call when caller ask you to do so", "transfers": {"Dave": "42"}, "voice": "alloy"}`

	var o CallOverride
	if err := json.Unmarshal([]byte(header), &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	base := Config{
		OpenAI: OpenAIConfig{
			Model:        "gpt-realtime",
			Voice:        "verse",
			SystemPrompt: "base prompt",
			APIKey:       "sk-server", // must survive: api_key is not overridable
		},
		Transfers: map[string]string{"Frontdesk": "sip:fd@example.com"},
	}
	got := base.WithOverride(&o)

	if got.OpenAI.Model != "gpt-realtime-2" {
		t.Errorf("model = %q, want gpt-realtime-2", got.OpenAI.Model)
	}
	if got.OpenAI.Voice != "alloy" {
		t.Errorf("voice = %q, want alloy", got.OpenAI.Voice)
	}
	if got.OpenAI.SystemPrompt != "you are IVR" {
		t.Errorf("prompt = %q, want 'you are IVR'", got.OpenAI.SystemPrompt)
	}
	if got.OpenAI.HangupToolDesc == "" || got.OpenAI.TransferToolDesc == "" {
		t.Errorf("tool descriptions not applied: %+v", got.OpenAI)
	}
	if got.OpenAI.APIKey != "sk-server" {
		t.Errorf("api_key changed to %q; must stay sk-server", got.OpenAI.APIKey)
	}

	// Bare number normalized to tel:, and the override replaces the base map.
	if got.Transfers["Dave"] != "tel:42" {
		t.Errorf("transfers[Dave] = %q, want tel:42", got.Transfers["Dave"])
	}
	if _, ok := got.Transfers["Frontdesk"]; ok {
		t.Errorf("override transfers should replace base map, found Frontdesk")
	}

	// Base config must be untouched (deep copy).
	if base.OpenAI.Model != "gpt-realtime" || base.Transfers["Frontdesk"] != "sip:fd@example.com" {
		t.Errorf("base config mutated: %+v / %v", base.OpenAI, base.Transfers)
	}
}

func TestNormalizeTransfers(t *testing.T) {
	m := map[string]string{
		"bare":  "42",
		"tel":   "tel:+15551234567",
		"sip":   "sip:sales@example.com",
		"sips":  "sips:secure@example.com",
		"space": "  99  ",
		"empty": "",
	}
	normalizeTransfers(m)

	want := map[string]string{
		"bare":  "tel:42",
		"tel":   "tel:+15551234567",
		"sip":   "sip:sales@example.com",
		"sips":  "sips:secure@example.com",
		"space": "tel:99",
		"empty": "",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("transfers[%s] = %q, want %q", k, m[k], v)
		}
	}
}

func TestWithOverrideNil(t *testing.T) {
	base := Config{Transfers: map[string]string{"a": "tel:1"}}
	got := base.WithOverride(nil)
	got.Transfers["a"] = "mutated"
	if base.Transfers["a"] != "tel:1" {
		t.Errorf("nil override must still deep-copy transfers; base mutated to %q", base.Transfers["a"])
	}
}
