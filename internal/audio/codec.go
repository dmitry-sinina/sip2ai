package audio

import (
	"github.com/zaf/g711"
)

// DecodeUlaw decodes G.711 µ-law bytes to PCM16 LE bytes.
// Returns the decoded bytes and their length.
func DecodeUlaw(dst, src []byte) (int, error) {
	out := g711.DecodeUlaw(src)
	n := copy(dst, out)
	return n, nil
}

// EncodeUlaw encodes PCM16 LE bytes to G.711 µ-law bytes.
// Returns the encoded bytes and their length.
func EncodeUlaw(dst, src []byte) (int, error) {
	out := g711.EncodeUlaw(src)
	n := copy(dst, out)
	return n, nil
}

// DecodeAlaw decodes G.711 A-law bytes to PCM16 LE bytes.
func DecodeAlaw(dst, src []byte) (int, error) {
	out := g711.DecodeAlaw(src)
	n := copy(dst, out)
	return n, nil
}

// EncodeAlaw encodes PCM16 LE bytes to G.711 A-law bytes.
func EncodeAlaw(dst, src []byte) (int, error) {
	out := g711.EncodeAlaw(src)
	n := copy(dst, out)
	return n, nil
}
