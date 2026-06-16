package sip

import (
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
