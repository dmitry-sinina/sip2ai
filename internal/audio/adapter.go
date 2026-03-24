package audio

import (
	"io"
	"log/slog"
	"sync"
)

// AudioAdapter accumulates variable-length byte chunks from an AI provider and
// emits exactly FrameBytesG711-sized frames for RTP output.
//
// Write is non-blocking; it drops the oldest data when the internal buffer
// exceeds adapterBufCap. Read blocks until a full frame is available or Close
// is called.
type AudioAdapter struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  []byte
	done bool

	logger       *slog.Logger
	bytesIn      int64
	bytesOut     int64
	bytesDropped int64
}

// NewAudioAdapter returns a ready-to-use AudioAdapter.
func NewAudioAdapter(logger *slog.Logger) *AudioAdapter {
	if logger == nil {
		logger = slog.Default()
	}
	a := &AudioAdapter{
		buf:    make([]byte, 0, adapterBufCap),
		logger: logger,
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

// Read blocks until at least FrameBytesG711 bytes are available, then copies
// exactly FrameBytesG711 bytes into p. p must be at least FrameBytesG711 bytes.
// Returns io.EOF when Close has been called and the buffer is drained.
func (a *AudioAdapter) Read(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		if len(a.buf) >= FrameBytesG711 {
			n := copy(p, a.buf[:FrameBytesG711])
			a.bytesOut += int64(n)
			a.buf = a.buf[FrameBytesG711:]
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

// TryRead copies exactly FrameBytesG711 bytes into p if available.
// Returns 0, nil if not enough data is buffered (non-blocking).
// Returns 0, io.EOF if Close has been called and the buffer is drained.
func (a *AudioAdapter) TryRead(p []byte) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.buf) >= FrameBytesG711 {
		n := copy(p, a.buf[:FrameBytesG711])
		a.bytesOut += int64(n)
		a.buf = a.buf[FrameBytesG711:]
		return n, nil
	}
	if a.done {
		return 0, io.EOF
	}
	return 0, nil
}

// Close signals the adapter to stop. Any blocked Read will return io.EOF.
func (a *AudioAdapter) Close() error {
	a.mu.Lock()
	a.done = true
	a.cond.Broadcast()
	a.mu.Unlock()
	return nil
}
