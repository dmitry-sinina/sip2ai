// Package metrics provides a nil-safe Prometheus metrics recorder for sip2ai.
// When the Recorder is nil (metrics disabled), all methods are no-ops.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type Recorder struct {
	callsActive   prometheus.Gauge
	callsTotal    *prometheus.CounterVec
	callDuration  prometheus.ObserverVec
	connectDur    prometheus.ObserverVec
	aiErrors      *prometheus.CounterVec
	tokens        *prometheus.CounterVec
	framesSent    prometheus.Counter
	framesRecv    prometheus.Counter
	hasFrameStats bool
}

func NewRecorder(reg prometheus.Registerer, logMedia bool) *Recorder {
	r := &Recorder{}

	r.callsActive = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "sip2ai_calls_active",
		Help: "Number of currently active calls.",
	})
	reg.MustRegister(r.callsActive)

	r.callsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sip2ai_calls_total",
		Help: "Total number of calls handled.",
	}, []string{"provider", "codec", "status"})
	reg.MustRegister(r.callsTotal)

	r.callDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sip2ai_call_duration_seconds",
		Help:    "Call duration in seconds.",
		Buckets: []float64{1, 5, 10, 30, 60, 120, 300, 600, 1800},
	}, []string{"provider"})
	reg.MustRegister(r.callDuration)

	r.connectDur = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "sip2ai_ai_connect_duration_seconds",
		Help:    "AI provider connection latency in seconds.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"provider"})
	reg.MustRegister(r.connectDur)

	r.aiErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sip2ai_ai_errors_total",
		Help: "Total AI provider errors.",
	}, []string{"provider", "error_type"})
	reg.MustRegister(r.aiErrors)

	r.tokens = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "sip2ai_tokens_total",
		Help: "Total tokens consumed by AI providers.",
	}, []string{"provider", "token_type"})
	reg.MustRegister(r.tokens)

	if logMedia {
		r.hasFrameStats = true
		r.framesSent = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sip2ai_audio_frames_sent_total",
			Help: "Total audio frames sent to AI providers.",
		})
		reg.MustRegister(r.framesSent)
		r.framesRecv = prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sip2ai_audio_frames_received_total",
			Help: "Total audio frames received from AI providers.",
		})
		reg.MustRegister(r.framesRecv)
	}

	return r
}

func (r *Recorder) CallStarted() {
	if r == nil {
		return
	}
	r.callsActive.Inc()
}

func (r *Recorder) CallEnded(provider, codec, status string, duration time.Duration) {
	if r == nil {
		return
	}
	r.callsActive.Dec()
	r.callsTotal.WithLabelValues(provider, codec, status).Inc()
	r.callDuration.WithLabelValues(provider).Observe(duration.Seconds())
}

func (r *Recorder) AIConnectDuration(provider string, d time.Duration) {
	if r == nil {
		return
	}
	r.connectDur.WithLabelValues(provider).Observe(d.Seconds())
}

func (r *Recorder) AIError(provider, errorType string) {
	if r == nil {
		return
	}
	r.aiErrors.WithLabelValues(provider, errorType).Inc()
}

func (r *Recorder) TokensUsed(provider string, inputText, inputAudio, outputText, outputAudio int) {
	if r == nil {
		return
	}
	r.tokens.WithLabelValues(provider, "input_text").Add(float64(inputText))
	r.tokens.WithLabelValues(provider, "input_audio").Add(float64(inputAudio))
	r.tokens.WithLabelValues(provider, "output_text").Add(float64(outputText))
	r.tokens.WithLabelValues(provider, "output_audio").Add(float64(outputAudio))
}

func (r *Recorder) AudioFrameSent() {
	if r == nil || !r.hasFrameStats {
		return
	}
	r.framesSent.Inc()
}

func (r *Recorder) AudioFrameReceived() {
	if r == nil || !r.hasFrameStats {
		return
	}
	r.framesRecv.Inc()
}
