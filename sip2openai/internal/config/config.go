// Package config loads sip2openai configuration from a YAML file with
// environment-variable overrides. Precedence: defaults < YAML < env.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	SIP    SIPConfig    `yaml:"sip"`
	OpenAI OpenAIConfig `yaml:"openai"`
	Log    LogConfig    `yaml:"log"`
	// Transfers maps a destination name (offered to the model as a transfer
	// target) to a SIP/tel URI. Empty disables the transfer_call tool.
	Transfers map[string]string `yaml:"transfers"`
}

type SIPConfig struct {
	BindHost     string `yaml:"bind_host"`
	BindPort     int    `yaml:"bind_port"`
	ExternalHost string `yaml:"external_host"` // Contact host (defaults to bind_host)
	ExternalPort int    `yaml:"external_port"` // Contact port (defaults to bind_port)
	// UDPMTU raises sipgo's UDP message cap so large WebRTC SDP answers fit in
	// the 200 OK; sends above the cap otherwise fail with ErrUDPMTUCongestion.
	// Above the real path MTU this relies on IP fragmentation.
	UDPMTU    int  `yaml:"udp_mtu"`
	EnableTCP bool `yaml:"enable_tcp"` // also listen on TCP for oversized messages
}

type OpenAIConfig struct {
	APIKey  string `yaml:"api_key"`  // or env OPENAI_API_KEY
	Model   string `yaml:"model"`    // default gpt-realtime
	BaseURL string `yaml:"base_url"` // default https://api.openai.com
	// Proxy routes all signaling requests through the given URL
	// (http/https/socks5). Empty falls back to HTTPS_PROXY/HTTP_PROXY env vars.
	// or env OPENAI_PROXY. Note: media flows caller<->OpenAI directly and is
	// never proxied.
	Proxy string `yaml:"proxy"`

	// Sideband session config (M2).
	Voice            string `yaml:"voice"`              // output voice, default alloy
	SystemPrompt     string `yaml:"system_prompt"`      // model instructions
	Greeting         string `yaml:"greeting"`           // spoken first if non-empty
	HangupToolDesc   string `yaml:"hangup_tool_desc"`   // hangup_call tool description
	TransferToolDesc string `yaml:"transfer_tool_desc"` // transfer_call tool description
}

type LogConfig struct {
	Level  string `yaml:"level"`  // base: trace|debug|info|warn|error
	Format string `yaml:"format"` // text|json
	// Per-component levels; empty falls back to Level. SIP controls the sipgo
	// stack (full SIP message dumps at debug, FSM traces at trace); OpenAI
	// controls sideband signaling (full WS event payloads at trace).
	SIP    string `yaml:"sip"`
	OpenAI string `yaml:"openai"`
}

// Default returns the baseline config before YAML/env are applied.
func Default() Config {
	return Config{
		SIP: SIPConfig{
			BindHost:  "0.0.0.0",
			BindPort:  5060,
			UDPMTU:    4096,
			EnableTCP: true,
		},
		OpenAI: OpenAIConfig{
			Model:            "gpt-realtime",
			BaseURL:          "https://api.openai.com",
			Voice:            "alloy",
			HangupToolDesc:   "End the call when the conversation is complete or the caller asks to hang up.",
			TransferToolDesc: "Transfer the caller to another department or person.",
		},
		Log: LogConfig{Level: "info", Format: "text"},
	}
}

// Load reads config from path (missing file is fine: defaults + env are used),
// then applies environment overrides.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(data, &cfg); err != nil {
				return cfg, fmt.Errorf("parse config %s: %w", path, err)
			}
		case !os.IsNotExist(err):
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	normalizeTransfers(cfg.Transfers)
	return cfg, nil
}

// CallOverride carries per-call settings parsed from the X-Sip2ai-Config SIP
// header (JSON). Only non-nil fields override the server config for that call.
// Field names match the header keys. provider is accepted for compatibility
// but ignored (sip2openai is OpenAI-only).
type CallOverride struct {
	Provider         *string           `json:"provider,omitempty"`
	Model            *string           `json:"model,omitempty"`
	Voice            *string           `json:"voice,omitempty"`
	SystemPrompt     *string           `json:"prompt,omitempty"`
	Greeting         *string           `json:"greeting,omitempty"`
	HangupToolDesc   *string           `json:"hangup_tool_desc,omitempty"`
	TransferToolDesc *string           `json:"transfer_tool_desc,omitempty"`
	Transfers        map[string]string `json:"transfers,omitempty"`
}

// normalizeTransfers rewrites bare phone numbers into tel: URIs in place.
// Values that already carry a URI scheme (e.g. "tel:", "sip:", "sips:") are
// left untouched; a bare value is detected by the absence of a ":" separator.
func normalizeTransfers(m map[string]string) {
	for k, v := range m {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if !strings.Contains(v, ":") {
			v = "tel:" + v
		}
		m[k] = v
	}
}

// WithOverride returns a copy of cfg with o applied. The Transfers map is
// deep-copied so the override never mutates the server's base config. A nil o
// yields an unmodified (but still deep-copied) config.
func (cfg Config) WithOverride(o *CallOverride) Config {
	c := cfg // copy; maps are shared until reassigned below
	if cfg.Transfers != nil {
		c.Transfers = make(map[string]string, len(cfg.Transfers))
		for k, v := range cfg.Transfers {
			c.Transfers[k] = v
		}
	}
	if o == nil {
		return c
	}
	if o.Model != nil {
		c.OpenAI.Model = *o.Model
	}
	if o.Voice != nil {
		c.OpenAI.Voice = *o.Voice
	}
	if o.SystemPrompt != nil {
		c.OpenAI.SystemPrompt = *o.SystemPrompt
	}
	if o.Greeting != nil {
		c.OpenAI.Greeting = *o.Greeting
	}
	if o.HangupToolDesc != nil {
		c.OpenAI.HangupToolDesc = *o.HangupToolDesc
	}
	if o.TransferToolDesc != nil {
		c.OpenAI.TransferToolDesc = *o.TransferToolDesc
	}
	if o.Transfers != nil {
		c.Transfers = o.Transfers
		normalizeTransfers(c.Transfers)
	}
	return c
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		cfg.OpenAI.APIKey = v
	}
	if v := os.Getenv("OPENAI_PROXY"); v != "" {
		cfg.OpenAI.Proxy = v
	}
	if v := os.Getenv("SIP_BIND_HOST"); v != "" {
		cfg.SIP.BindHost = v
	}
	if v := os.Getenv("SIP_EXTERNAL_HOST"); v != "" {
		cfg.SIP.ExternalHost = v
	}
}
