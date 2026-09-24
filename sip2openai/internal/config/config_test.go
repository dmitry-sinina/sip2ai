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
			APIKey:       "sk-server", // must survive: this header carries no api_key
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
		t.Errorf("api_key changed to %q; must stay sk-server when the header omits it", got.OpenAI.APIKey)
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

func TestWithOverrideAPIKey(t *testing.T) {
	var o CallOverride
	if err := json.Unmarshal([]byte(`{"api_key":"sk-caller","voice":"verse"}`), &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	base := Config{OpenAI: OpenAIConfig{APIKey: "sk-server", Voice: "alloy"}}
	got := base.WithOverride(&o)

	if got.OpenAI.APIKey != "sk-caller" {
		t.Errorf("api_key = %q, want sk-caller", got.OpenAI.APIKey)
	}
	if got.OpenAI.Voice != "verse" {
		t.Errorf("voice = %q, want verse", got.OpenAI.Voice)
	}
	if base.OpenAI.APIKey != "sk-server" {
		t.Errorf("base api_key mutated to %q", base.OpenAI.APIKey)
	}

	t.Run("surrounding whitespace is trimmed", func(t *testing.T) {
		padded := " \tsk-caller \n"
		got := base.WithOverride(&CallOverride{APIKey: &padded})
		if got.OpenAI.APIKey != "sk-caller" {
			t.Errorf("api_key = %q, want sk-caller with the padding stripped", got.OpenAI.APIKey)
		}
	})
}

func TestWithOverrideGreetingDelay(t *testing.T) {
	base := Default()
	if base.OpenAI.GreetingDelayMs != 1000 {
		t.Fatalf("default greeting_delay_ms = %d, want 1000", base.OpenAI.GreetingDelayMs)
	}

	var o CallOverride
	if err := json.Unmarshal([]byte(`{"greeting_delay_ms":250}`), &o); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := base.WithOverride(&o).OpenAI.GreetingDelayMs; got != 250 {
		t.Errorf("greeting_delay_ms = %d, want 250", got)
	}

	// Explicit zero must win over the default, not be treated as "unset".
	zero := 0
	if got := base.WithOverride(&CallOverride{GreetingDelayMs: &zero}).OpenAI.GreetingDelayMs; got != 0 {
		t.Errorf("greeting_delay_ms = %d, want 0 from an explicit override", got)
	}
	if base.OpenAI.GreetingDelayMs != 1000 {
		t.Errorf("base greeting_delay_ms mutated to %d", base.OpenAI.GreetingDelayMs)
	}
}

func TestCallOverrideValidate(t *testing.T) {
	str := func(s string) *string { return &s }
	num := func(n int) *int { return &n }
	cases := []struct {
		name    string
		o       *CallOverride
		wantErr bool
	}{
		{"nil override", nil, false},
		{"no api_key", &CallOverride{Voice: str("alloy")}, false},
		{"api_key set", &CallOverride{APIKey: str("sk-caller")}, false},
		{"api_key empty", &CallOverride{APIKey: str("")}, true},
		{"api_key blank", &CallOverride{APIKey: str("  \t ")}, true},
		{"greeting_delay_ms zero", &CallOverride{GreetingDelayMs: num(0)}, false},
		{"greeting_delay_ms positive", &CallOverride{GreetingDelayMs: num(1500)}, false},
		{"greeting_delay_ms negative", &CallOverride{GreetingDelayMs: num(-1)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.o.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}
