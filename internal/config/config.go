package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	SIP       SIPConfig         `yaml:"sip"`
	AI        AIConfig          `yaml:"ai"`
	Log       LogConfig         `yaml:"log"`
	OpenAI    OpenAIConfig      `yaml:"openai"`
	Deepgram  DeepgramConfig    `yaml:"deepgram"`
	Gemini    GeminiConfig      `yaml:"gemini"`
	Transfers map[string]string `yaml:"transfers"`
}

type LogConfig struct {
	Format   string `yaml:"format"`
	Level    string `yaml:"level"`
	SIP      string `yaml:"sip"`
	OpenAI   string `yaml:"openai"`
	Deepgram string `yaml:"deepgram"`
	Gemini   string `yaml:"gemini"`
	Media    bool   `yaml:"media"`
}

type SIPConfig struct {
	BindHost        string `yaml:"bind_host"`
	BindPort        int    `yaml:"bind_port"`
	Transport       string `yaml:"transport"`
	ExternalHost    string `yaml:"external_host"`
	ExternalPort    int    `yaml:"external_port"`
	MediaExternalIP string `yaml:"media_external_ip"`
}

type AIConfig struct {
	Provider          string `yaml:"provider"`
	ConnectTimeoutMs  int    `yaml:"connect_timeout_ms"`
	ReconnectRetries  int    `yaml:"reconnect_retries"`
	ReconnectDelayMs  int    `yaml:"reconnect_delay_ms"`
	DumpAudio         bool   `yaml:"dump_audio"`
	LogMedia          bool   `yaml:"-"` // set via --log-media CLI flag
}

type OpenAIConfig struct {
	APIKey       string `yaml:"api_key"`
	Model        string `yaml:"model"`
	Voice        string `yaml:"voice"`
	SystemPrompt string `yaml:"system_prompt"`
	Greeting     string `yaml:"greeting"`
	Proxy        string `yaml:"proxy"`
}

type DeepgramConfig struct {
	APIKey       string `yaml:"api_key"`
	ListenModel  string `yaml:"listen_model"`
	ThinkModel   string `yaml:"think_model"`
	SpeakModel   string `yaml:"speak_model"`
	Language     string `yaml:"language"`
	Greeting     string `yaml:"greeting"`
	Proxy        string `yaml:"proxy"`
}

type GeminiConfig struct {
	APIKey       string `yaml:"api_key"`
	Model        string `yaml:"model"`
	SystemPrompt string `yaml:"system_prompt"`
	Proxy        string `yaml:"proxy"`
}

// CallOverride contains per-call settings from the X-Sip2ai-Config SIP header.
// Only non-nil fields override the YAML defaults.
type CallOverride struct {
	Provider     *string           `json:"provider,omitempty"`
	APIKey       *string           `json:"api_key,omitempty"`
	Model        *string           `json:"model,omitempty"`
	Voice        *string           `json:"voice,omitempty"`
	SystemPrompt *string           `json:"system_prompt,omitempty"`
	Greeting     *string           `json:"greeting,omitempty"`
	ListenModel  *string           `json:"listen_model,omitempty"`
	ThinkModel   *string           `json:"think_model,omitempty"`
	SpeakModel   *string           `json:"speak_model,omitempty"`
	Language     *string           `json:"language,omitempty"`
	Transfers    map[string]string `json:"transfers,omitempty"`
}

// WithOverride returns a deep copy of cfg with the override applied.
// If o is nil, returns an unmodified copy.
func (cfg *Config) WithOverride(o *CallOverride) *Config {
	c := *cfg // shallow copy
	// Deep-copy transfers map.
	if cfg.Transfers != nil {
		c.Transfers = make(map[string]string, len(cfg.Transfers))
		for k, v := range cfg.Transfers {
			c.Transfers[k] = v
		}
	}
	if o == nil {
		return &c
	}
	if o.Provider != nil {
		c.AI.Provider = *o.Provider
	}
	if o.Transfers != nil {
		c.Transfers = o.Transfers
	}
	// Apply to the active provider config.
	if o.APIKey != nil {
		c.OpenAI.APIKey = *o.APIKey
		c.Deepgram.APIKey = *o.APIKey
		c.Gemini.APIKey = *o.APIKey
	}
	if o.SystemPrompt != nil {
		c.OpenAI.SystemPrompt = *o.SystemPrompt
		c.Gemini.SystemPrompt = *o.SystemPrompt
	}
	if o.Model != nil {
		c.OpenAI.Model = *o.Model
		c.Gemini.Model = *o.Model
	}
	if o.Voice != nil {
		c.OpenAI.Voice = *o.Voice
	}
	if o.Greeting != nil {
		c.OpenAI.Greeting = *o.Greeting
		c.Deepgram.Greeting = *o.Greeting
	}
	// Deepgram-specific.
	if o.ListenModel != nil {
		c.Deepgram.ListenModel = *o.ListenModel
	}
	if o.ThinkModel != nil {
		c.Deepgram.ThinkModel = *o.ThinkModel
	}
	if o.SpeakModel != nil {
		c.Deepgram.SpeakModel = *o.SpeakModel
	}
	if o.Language != nil {
		c.Deepgram.Language = *o.Language
	}
	return &c
}

func Load(path string) (*Config, error) {
	cfg := &Config{}
	// defaults
	cfg.SIP.BindHost = "0.0.0.0"
	cfg.SIP.BindPort = 5060
	cfg.SIP.Transport = "udp"
	cfg.Log.Format = "text"
	cfg.Log.Level = "warn"
	cfg.AI.Provider = "openai"
	cfg.AI.ConnectTimeoutMs = 5000
	cfg.AI.ReconnectRetries = 3
	cfg.AI.ReconnectDelayMs = 1000
	cfg.OpenAI.Model = "gpt-4o-realtime-preview"
	cfg.OpenAI.Voice = "alloy"
	cfg.OpenAI.SystemPrompt = "You are a helpful voice assistant."
	cfg.Deepgram.ListenModel = "nova-3"
	cfg.Deepgram.ThinkModel = "gpt-4o-mini"
	cfg.Deepgram.SpeakModel = "aura-2-thalia-en"
	cfg.Deepgram.Language = "en"
	cfg.Deepgram.Greeting = "Hello, how can I help you?"
	cfg.Gemini.Model = "gemini-2.5-flash-native-audio-preview-12-2025"
	cfg.Gemini.SystemPrompt = "You are a helpful voice assistant."

	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			if err := yaml.Unmarshal(data, cfg); err != nil {
				return nil, fmt.Errorf("parse config: %w", err)
			}
		}
	}

	// Environment variables override config file.
	if v := os.Getenv("SIP_BIND_HOST"); v != "" {
		cfg.SIP.BindHost = v
	}
	if v := os.Getenv("SIP_EXTERNAL_HOST"); v != "" {
		cfg.SIP.ExternalHost = v
	}
	if v := os.Getenv("SIP_MEDIA_EXTERNAL_IP"); v != "" {
		cfg.SIP.MediaExternalIP = v
	}
	if v := os.Getenv("OPENAI_API_KEY"); v != "" {
		cfg.OpenAI.APIKey = v
	}
	if v := os.Getenv("DEEPGRAM_API_KEY"); v != "" {
		cfg.Deepgram.APIKey = v
	}
	if v := os.Getenv("GEMINI_API_KEY"); v != "" {
		cfg.Gemini.APIKey = v
	}

	return cfg, nil
}
