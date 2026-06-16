// Package openai is a thin client for OpenAI's Realtime "calls" (WebRTC/WHIP)
// endpoint. sip2openai relays the caller's SDP offer here and hands the answer
// back; media itself flows directly between the caller and OpenAI.
package openai

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const defaultBaseURL = "https://api.openai.com"

// Client posts SDP offers to the Realtime calls endpoint and controls calls.
type Client struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
	// proxyTransport is non-nil only when an explicit proxy was configured; it
	// is shared with the sideband WebSocket dial so both the SDP call and the
	// control plane route through the same proxy. nil = use Go's default
	// transport (which still honors HTTPS_PROXY/HTTP_PROXY/NO_PROXY).
	proxyTransport *http.Transport
}

// New builds a calls client. Empty model/baseURL fall back to GA defaults.
// proxy, if non-empty, is the URL all signaling requests (the SDP call and the
// sideband control WebSocket) are routed through (e.g. http://host:3128 or
// socks5://host:1080); empty leaves Go's default transport in place, which
// still honors HTTPS_PROXY/HTTP_PROXY/NO_PROXY.
func New(apiKey, model, baseURL, proxy string) (*Client, error) {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if model == "" {
		model = "gpt-realtime"
	}
	httpClient := &http.Client{Timeout: 20 * time.Second}
	var transport *http.Transport
	if proxy != "" {
		u, err := url.Parse(proxy)
		if err != nil {
			return nil, fmt.Errorf("parse openai proxy %q: %w", proxy, err)
		}
		transport = &http.Transport{Proxy: http.ProxyURL(u)}
		httpClient.Transport = transport
	}
	return &Client{
		APIKey:         apiKey,
		Model:          model,
		BaseURL:        strings.TrimRight(baseURL, "/"),
		HTTP:           httpClient,
		proxyTransport: transport,
	}, nil
}

// CreateCall posts a WebRTC SDP offer (Content-Type: application/sdp) and
// returns OpenAI's SDP answer plus the call_id parsed from the Location header.
// The call_id is the handle for the sideband control WebSocket (added in M2).
// model overrides the client default for this call (empty = client default),
// used for the per-call X-Sip2ai-Config override.
func (c *Client) CreateCall(ctx context.Context, offer []byte, model string) (answer []byte, callID string, err error) {
	if model == "" {
		model = c.Model
	}
	url := fmt.Sprintf("%s/v1/realtime/calls?model=%s", c.BaseURL, model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(offer))
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/sdp")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("post offer: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, "", &APIError{Status: resp.StatusCode, Body: string(body)}
	}
	return body, callIDFromLocation(resp.Header.Get("Location")), nil
}

// Hangup ends an active call by call_id (best-effort cleanup on SIP BYE).
func (c *Client) Hangup(ctx context.Context, callID string) error {
	if callID == "" {
		return nil
	}
	url := fmt.Sprintf("%s/v1/realtime/calls/%s/hangup", c.BaseURL, callID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return &APIError{Status: resp.StatusCode, Body: string(body)}
	}
	return nil
}

func callIDFromLocation(loc string) string {
	loc = strings.TrimSpace(loc)
	if loc == "" {
		return ""
	}
	return path.Base(strings.TrimRight(loc, "/"))
}

// APIError carries a non-2xx response from the calls endpoint.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openai calls API: HTTP %d: %s", e.Status, e.Body)
}
