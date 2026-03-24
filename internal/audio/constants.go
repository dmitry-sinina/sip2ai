package audio

const (
	// FrameSamples is the number of PCM samples per 20ms RTP frame at 8kHz.
	FrameSamples = 160
	// FrameBytesPCM is the byte length of one PCM16 LE frame (160 samples × 2 bytes).
	FrameBytesPCM = 320
	// FrameBytesG711 is the byte length of one G.711 encoded frame (160 bytes).
	FrameBytesG711 = 160
	// adapterBufCap is the internal buffer capacity. Must be large enough to
	// absorb OpenAI's faster-than-realtime audio bursts while diago drains
	// at 8000 bytes/sec (G.711 8kHz). 80000 bytes = 10 seconds of audio.
	adapterBufCap = 80000
)
