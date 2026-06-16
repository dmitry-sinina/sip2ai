package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCreateCallSuccess(t *testing.T) {
	var gotMethod, gotPath, gotModel, gotAuth, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotModel = r.URL.Query().Get("model")
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Location", "/v1/realtime/calls/rtc_abc123")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("answer-sdp"))
	}))
	defer srv.Close()

	c, err := New("sk-test", "gpt-realtime", srv.URL, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	answer, callID, err := c.CreateCall(context.Background(), []byte("offer-sdp"), "")
	if err != nil {
		t.Fatalf("CreateCall: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if gotPath != "/v1/realtime/calls" {
		t.Errorf("path = %s", gotPath)
	}
	if gotModel != "gpt-realtime" {
		t.Errorf("model query = %q, want gpt-realtime (client default)", gotModel)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("auth = %q", gotAuth)
	}
	if gotCT != "application/sdp" {
		t.Errorf("content-type = %q", gotCT)
	}
	if string(gotBody) != "offer-sdp" {
		t.Errorf("body = %q", gotBody)
	}
	if string(answer) != "answer-sdp" {
		t.Errorf("answer = %q", answer)
	}
	if callID != "rtc_abc123" {
		t.Errorf("callID = %q, want rtc_abc123", callID)
	}
}

func TestCreateCallModelOverride(t *testing.T) {
	var gotModel string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotModel = r.URL.Query().Get("model")
		w.Header().Set("Location", "/v1/realtime/calls/x")
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	c, _ := New("k", "gpt-realtime", srv.URL, "")
	if _, _, err := c.CreateCall(context.Background(), []byte("o"), "gpt-realtime-2"); err != nil {
		t.Fatalf("CreateCall: %v", err)
	}
	if gotModel != "gpt-realtime-2" {
		t.Errorf("model query = %q, want per-call override gpt-realtime-2", gotModel)
	}
}

func TestCreateCallError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad offer"))
	}))
	defer srv.Close()

	c, _ := New("k", "m", srv.URL, "")
	_, _, err := c.CreateCall(context.Background(), []byte("o"), "")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if apiErr.Status != http.StatusBadRequest || apiErr.Body != "bad offer" {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestHangup(t *testing.T) {
	t.Run("empty callID makes no request", func(t *testing.T) {
		hit := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
		defer srv.Close()
		c, _ := New("k", "m", srv.URL, "")
		if err := c.Hangup(context.Background(), ""); err != nil {
			t.Fatalf("Hangup: %v", err)
		}
		if hit {
			t.Error("server should not be called for empty callID")
		}
	})

	t.Run("success", func(t *testing.T) {
		var gotPath string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()
		c, _ := New("k", "m", srv.URL, "")
		if err := c.Hangup(context.Background(), "rtc_9"); err != nil {
			t.Fatalf("Hangup: %v", err)
		}
		if gotPath != "/v1/realtime/calls/rtc_9/hangup" {
			t.Errorf("path = %s", gotPath)
		}
	})

	t.Run("error maps to APIError", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		c, _ := New("k", "m", srv.URL, "")
		var apiErr *APIError
		if err := c.Hangup(context.Background(), "rtc_9"); !errors.As(err, &apiErr) {
			t.Fatalf("err = %v, want *APIError", err)
		}
	})
}

func TestCallIDFromLocation(t *testing.T) {
	tests := map[string]string{
		"https://api.openai.com/v1/realtime/calls/rtc_abc": "rtc_abc",
		"/v1/realtime/calls/rtc_xyz/":                      "rtc_xyz",
		"rtc_bare":                                         "rtc_bare",
		"  ":                                               "",
		"":                                                 "",
	}
	for loc, want := range tests {
		if got := callIDFromLocation(loc); got != want {
			t.Errorf("callIDFromLocation(%q) = %q, want %q", loc, got, want)
		}
	}
}

func TestNewProxy(t *testing.T) {
	if _, err := New("k", "m", "", "://bad-url"); err == nil {
		t.Error("expected error for invalid proxy URL")
	}
	c, err := New("k", "m", "", "http://proxy.example:3128")
	if err != nil {
		t.Fatalf("New with valid proxy: %v", err)
	}
	if c.proxyTransport == nil || c.HTTP.Transport == nil {
		t.Error("explicit proxy should install a Transport")
	}
}

// TestCreateCallViaProxy verifies signaling requests actually route through the
// configured proxy (the proxy receives the request and its response is used).
func TestCreateCallViaProxy(t *testing.T) {
	var proxied bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied = true
		if r.Host != "upstream.invalid" {
			t.Errorf("proxy got host %q, want upstream.invalid", r.Host)
		}
		w.Header().Set("Location", "/v1/realtime/calls/rtc_proxied")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("a"))
	}))
	defer proxy.Close()

	c, err := New("k", "m", "http://upstream.invalid", proxy.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, callID, err := c.CreateCall(context.Background(), []byte("o"), "")
	if err != nil {
		t.Fatalf("CreateCall: %v", err)
	}
	if !proxied {
		t.Error("request did not go through the proxy")
	}
	if callID != "rtc_proxied" {
		t.Errorf("callID = %q", callID)
	}
}
