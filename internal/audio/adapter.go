package audio

import (
	"io"
	"log/slog"
	"sync"
)

// AudioAdapter accumulates variable-length byte chunks from an AI provider and
// emits exactly frameBytes-sized frames for RTP output. The frame size depends
// on the negotiated codec (e.g. 160 for G.711 @8k, 960 for L16 @24k).
//
// Read blocks until a full frame is available or Close is called.
type AudioAdapter struct {
	mu         sync.Mutex
	cond       *sync.Cond
	buf        []byte
	done       bool
	frameBytes int

	logger       *slog.Logger
	bytesIn      int64
	bytesOut     int64
	bytesDropped int64
}

// NewAudioAdapter returns a ready-to-use AudioAdapter. frameBytes is the
// emit size (one 20ms RTP frame for the negotiated codec); must be > 0.
func NewAudioAdapter(logger *slog.Logger, frameBytes int) *AudioAdapter {
	if logger == nil {
		logger = slog.Default()
	}
	if frameBytes <= 0 {
		frameBytes = FrameBytesG711
	}
	a := &AudioAdapter{
		buf:        make([]byte, 0, adapterBufCap),
		logger:     logger,
		frameBytes: frameBytes,
	}
	a.cond = sync.NewCond(&a.mu)
	return a
}

// Write appends p to the internal buffer. If the buffer would exceed
// adapterBufCap, the oldest bytes are discarded to make room.
func (a *AudioAdapter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	a.mu.Lock()
	if a.done {
		a.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	a.bytesIn += int64(len(p))
	a.buf = append(a.buf, p...)
	a.cond.Signal()
	a.mu.Unlock()
	return len(p), nil
}

// Read blocks until at least frameBytes are available, then copies exactly
// frameBytes into p. p must be at least frameBytes long. Returns io.EOF when
// Close has been called and the buffer is drained.
func (a *AudioAdapter) Read(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		if len(a.buf) >= a.frameBytes {
			n := copy(p, a.buf[:a.frameBytes])
			a.bytesOut += int64(n)
			a.buf = a.buf[a.frameBytes:]
			return n, nil
		}
		if a.done {
			remainder := len(a.buf)
			a.logger.Info("adapter: closing",
				"bytes_in", a.bytesIn,
				"bytes_out", a.bytesOut,
				"bytes_dropped", a.bytesDropped,
				"remainder", remainder,
			)
			return 0, io.EOF
		}
		a.cond.Wait()
	}
}

// TryRead copies exactly frameBytes into p if available.
// Returns 0, nil if not enough data is buffered (non-blocking).
// Returns 0, io.EOF if Close has been called and the buffer is drained.
func (a *AudioAdapter) TryRead(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.buf) >= a.frameBytes {
		n := copy(p, a.buf[:a.frameBytes])
		a.bytesOut += int64(n)
		a.buf = a.buf[a.frameBytes:]
		return n, nil
	}
	if a.done {
		return 0, io.EOF
	}
	return 0, nil
}

// Drain discards all buffered audio without closing the adapter. Used for
// caller barge-in: when the AI detects the caller has started speaking, we
// flush any pre-buffered TTS so the caller isn't talked over by audio the
// model already generated. Returns the number of bytes dropped.
func (a *AudioAdapter) Drain() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := len(a.buf)
	a.buf = a.buf[:0]
	a.bytesDropped += int64(n)
	return n
}

// Close signals the adapter to stop. Any blocked Read will return io.EOF.
func (a *AudioAdapter) Close() error {
	a.mu.Lock()
	a.done = true
	a.cond.Broadcast()
	a.mu.Unlock()
	return nil
}
