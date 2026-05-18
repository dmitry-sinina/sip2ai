package ai

import (
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
)

// Custom codec definitions not provided by diago.
var (
	CodecL16_16000 = media.Codec{PayloadType: 96, SampleRate: 16000, SampleDur: 20 * time.Millisecond, NumChannels: 1, Name: "L16"}
	CodecL16_24000 = media.Codec{PayloadType: 97, SampleRate: 24000, SampleDur: 20 * time.Millisecond, NumChannels: 1, Name: "L16"}
)

// ProviderCodecs returns the preferred codec list for the given provider,
// ordered by preference (best match first, G.711 fallback last).
func ProviderCodecs(provider string) []media.Codec {
	switch ProviderType(provider) {
	case ProviderOpenAI:
		// OpenAI supports g711_ulaw, g711_alaw, pcm16 (24kHz).
		// Prefer L16/24000 (zero transcoding), fall back to G.711 (also zero).
		return []media.Codec{
			CodecL16_24000,
			media.CodecAudioUlaw,
			media.CodecAudioAlaw,
		}
	case ProviderDeepgram:
		// Deepgram supports mulaw, alaw, linear16 at 8/16/24kHz.
		// Prefer L16/24000, fall back to G.711.
		return []media.Codec{
			CodecL16_24000,
			media.CodecAudioUlaw,
			media.CodecAudioAlaw,
		}
	case ProviderGemini:
		// Gemini: send PCM16 16kHz, recv PCM16 24kHz.
		// Prefer L16/16000 (zero transcoding on send), then L16/24000
		// (zero transcoding on recv), fall back to G.711.
		return []media.Codec{
			CodecL16_16000,
			CodecL16_24000,
			media.CodecAudioUlaw,
			media.CodecAudioAlaw,
		}
	default:
		return []media.Codec{
			media.CodecAudioUlaw,
			media.CodecAudioAlaw,
		}
	}
}

// NegotiateCodec parses the remote SDP offer and returns the best matching
// codec from the provider's preferred list. Returns the matched codec and
// true, or a zero codec and false if no match.
func NegotiateCodec(provider string, sdpBody []byte) (media.Codec, bool) {
	sd := sdp.SessionDescription{}
	if err := sdp.Unmarshal(sdpBody, &sd); err != nil {
		return media.Codec{}, false
	}

	md, err := sd.MediaDescription("audio")
	if err != nil {
		return media.Codec{}, false
	}

	// Parse all codecs offered by the remote side.
	remoteCodecs := make([]media.Codec, len(md.Formats))
	n, _ := media.CodecsFromSDPRead(md.Formats, sd.Values("a"), remoteCodecs)
	remoteCodecs = remoteCodecs[:n]

	// Match against our preferred list (order = priority).
	preferred := ProviderCodecs(provider)
	for _, ours := range preferred {
		for _, theirs := range remoteCodecs {
			if codecMatch(ours, theirs) {
				// Use the remote payload type (important for dynamic PTs).
				ours.PayloadType = theirs.PayloadType
				return ours, true
			}
		}
	}
	return media.Codec{}, false
}

func codecMatch(a, b media.Codec) bool {
	return a.Name == b.Name && a.SampleRate == b.SampleRate
}
