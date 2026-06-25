package sip

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/emiago/sipgo/sip"

	"sip2openai/internal/config"
)

func TestParseConfigHeader(t *testing.T) {
	mkReq := func(headerVal string, set bool) *sip.Request {
		req := sip.NewRequest(sip.INVITE, sip.Uri{User: "x", Host: "127.0.0.1", Port: 5060})
		if set {
			req.AppendHeader(sip.NewHeader(configHeader, headerVal))
		}
		return req
	}

	t.Run("absent header returns nil,nil", func(t *testing.T) {
		o, err := parseConfigHeader(mkReq("", false))
		if err != nil || o != nil {
			t.Fatalf("got (%v, %v), want (nil, nil)", o, err)
		}
	})

	t.Run("valid JSON parses fields", func(t *testing.T) {
		o, err := parseConfigHeader(mkReq(`{"prompt":"be brief","voice":"verse","transfers":{"Dave":"42"}}`, true))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if o.SystemPrompt == nil || *o.SystemPrompt != "be brief" {
			t.Errorf("prompt = %v", o.SystemPrompt)
		}
		if o.Voice == nil || *o.Voice != "verse" {
			t.Errorf("voice = %v", o.Voice)
		}
		if o.Transfers["Dave"] != "42" {
			t.Errorf("transfers = %v", o.Transfers)
		}
	})

	t.Run("malformed JSON errors", func(t *testing.T) {
		if _, err := parseConfigHeader(mkReq(`{not json`, true)); err == nil {
			t.Error("expected error for malformed JSON")
		}
	})
}

func TestErrorDetailsJSON(t *testing.T) {
	t.Run("serializes error message", func(t *testing.T) {
		got := errorDetailsJSON(errors.New("parse X-Sip2ai-Config: unexpected end of JSON input"))
		var m map[string]string
		if err := json.Unmarshal([]byte(got), &m); err != nil {
			t.Fatalf("result is not valid JSON: %q: %v", got, err)
		}
		if m["error"] != "parse X-Sip2ai-Config: unexpected end of JSON input" {
			t.Errorf("error field = %q", m["error"])
		}
	})

	t.Run("strips CRLF so the value stays a single SIP header line", func(t *testing.T) {
		got := errorDetailsJSON(errors.New("line one\r\nline two"))
		if strings.ContainsAny(got, "\r\n") {
			t.Errorf("value contains CR/LF: %q", got)
		}
		// Still valid JSON after stripping.
		if err := json.Unmarshal([]byte(got), &map[string]string{}); err != nil {
			t.Errorf("not valid JSON after CRLF strip: %q: %v", got, err)
		}
	})

	t.Run("escapes quotes in the message", func(t *testing.T) {
		got := errorDetailsJSON(errors.New(`bad "quoted" value`))
		var m map[string]string
		if err := json.Unmarshal([]byte(got), &m); err != nil {
			t.Fatalf("result is not valid JSON: %q: %v", got, err)
		}
		if m["error"] != `bad "quoted" value` {
			t.Errorf("error field = %q", m["error"])
		}
	})
}

func TestControlOpts(t *testing.T) {
	s := &Server{}
	oaiCfg := config.OpenAIConfig{
		Voice:            "alloy",
		SystemPrompt:     "prompt",
		Greeting:         "hi",
		HangupToolDesc:   "hang",
		TransferToolDesc: "xfer",
	}
	transfers := map[string]string{"Dave": "tel:42"}

	opts := s.controlOpts(oaiCfg, transfers)
	if opts.Voice != "alloy" || opts.Instructions != "prompt" || opts.Greeting != "hi" {
		t.Errorf("opts = %+v", opts)
	}
	if opts.HangupDesc != "hang" || opts.TransferDesc != "xfer" {
		t.Errorf("tool descs = %+v", opts)
	}
	if opts.Transfers["Dave"] != "tel:42" {
		t.Errorf("transfers = %v", opts.Transfers)
	}
}
