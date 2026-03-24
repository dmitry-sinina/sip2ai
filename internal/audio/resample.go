package audio

import (
	"encoding/binary"
	"fmt"
)

// Resample8to16 upsamples 8 kHz PCM16 to 16 kHz PCM16 using linear interpolation.
// dst must have length 2*len(src).
func Resample8to16(dst, src []int16) error {
	if len(dst) != 2*len(src) {
		return fmt.Errorf("resample8to16: dst len %d must be 2×src len %d", len(dst), len(src))
	}
	for i := 0; i < len(src)-1; i++ {
		dst[2*i] = src[i]
		dst[2*i+1] = int16((int32(src[i]) + int32(src[i+1])) / 2)
	}
	// last sample
	if len(src) > 0 {
		last := len(src) - 1
		dst[2*last] = src[last]
		dst[2*last+1] = src[last]
	}
	return nil
}

// Resample24to8 downsamples 24 kHz PCM16 to 8 kHz PCM16 by taking every 3rd sample.
// dst must have length len(src)/3.
func Resample24to8(dst, src []int16) error {
	expected := len(src) / 3
	if len(dst) < expected {
		return fmt.Errorf("resample24to8: dst len %d too small for src len %d (need %d)", len(dst), len(src), expected)
	}
	for i := range expected {
		dst[i] = src[i*3]
	}
	return nil
}

// BytesToInt16 reinterprets a byte slice as a slice of little-endian int16.
// len(b) must be even.
func BytesToInt16(b []byte) ([]int16, error) {
	if len(b)%2 != 0 {
		return nil, fmt.Errorf("BytesToInt16: odd byte length %d", len(b))
	}
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return out, nil
}

// Int16ToBytes converts a slice of int16 to little-endian bytes.
func Int16ToBytes(s []int16) []byte {
	out := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
	}
	return out
}
