package audio

const (
	// FrameSamples is the number of PCM samples per 20ms RTP frame at 8kHz.
	FrameSamples = 160
	// FrameBytesPCM is the byte length of one PCM16 LE frame (160 samples × 2 bytes).
	FrameBytesPCM = 320
	// FrameBytesG711 is the byte length of one G.711 encoded frame (160 bytes).
	FrameBytesG711 = 160
	// adapterBufCap is the initial buffer capacity. Must be large enough to
	// absorb OpenAI's faster-than-realtime audio bursts while diago drains
	// at the negotiated codec's byte rate. Not a hard cap.
	adapterBufCap = 80000
	// FrameDurationMs is the RTP packetization interval used throughout.
	FrameDurationMs = 20
)

// FrameBytesFor returns the byte length of one 20ms frame for the given
// codec at the given sample rate. Examples:
//   PCMU/PCMA @ 8000 Hz → 160 bytes
//   L16      @ 8000 Hz → 320 bytes
//   L16      @ 16000 Hz → 640 bytes
//   L16      @ 24000 Hz → 960 bytes
func FrameBytesFor(codec string, rate uint32) int {
	return int(rate) * BytesPerSample(codec) * FrameDurationMs / 1000
}

// BytesPerSample returns the on-the-wire bytes per audio sample for the codec.
func BytesPerSample(codec string) int {
	if codec == "L16" {
		return 2
	}
	return 1 // PCMU, PCMA
}

// SilenceByte returns the single-byte silence pattern for the codec, suitable
// for filling a frame buffer.
//   PCMU → 0xFF (μ-law digital silence)
//   PCMA → 0xD5 (A-law digital silence)
//   L16  → 0x00 (zero sample)
func SilenceByte(codec string) byte {
	switch codec {
	case "PCMA":
		return 0xD5
	case "L16":
		return 0x00
	default:
		return 0xFF
	}
}

// SwapPCM16 byte-swaps each 16-bit sample in place. Used to convert between
// RTP L16 (big-endian per RFC 3551 §4.5.11) and the little-endian PCM that
// every AI backend expects. The conversion is symmetric — the same call
// converts BE→LE and LE→BE.
func SwapPCM16(b []byte) {
	n := len(b) &^ 1 // ignore trailing odd byte if any
	for i := 0; i < n; i += 2 {
		b[i], b[i+1] = b[i+1], b[i]
	}
}
