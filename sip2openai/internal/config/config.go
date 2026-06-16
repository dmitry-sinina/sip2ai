// Package config loads sip2openai configuration from a YAML file with
// environment-variable overrides. Precedence: defaults < YAML < env.
package config

import (
	"fmt"
	"os"

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

	// Sideband session config (M2).
	Voice            string `yaml:"voice"`              // output voice, default alloy
	SystemPrompt     string `yaml:"system_prompt"`      // model instructions
	Greeting         string `yaml:"greeting"`           // spoken first if non-empty
	HangupToolDesc   string `yaml:"hangup_tool_desc"`   // hangup_call tool description
	TransferToolDesc string `yaml:"transfer_tool_desc"` // transfer_call tool description
}

type LogConfig struct {
	Level  string `yaml:"level"`  // debug|info|warn|error
	Format string `yaml:"format"` // text|json
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
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		cfg.OpenAI.APIKey = v
	}
	if v := os.Getenv("SIP_BIND_HOST"); v != "" {
		cfg.SIP.BindHost = v
	}
	if v := os.Getenv("SIP_EXTERNAL_HOST"); v != "" {
		cfg.SIP.ExternalHost = v
	}
}
