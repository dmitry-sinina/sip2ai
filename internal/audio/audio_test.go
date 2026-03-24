package audio

import (
	"io"
	"sync"
	"testing"
)

func TestUlawRoundTrip(t *testing.T) {
	// Generate 160 samples of PCM16 as a sine-ish pattern.
	src := make([]byte, FrameBytesPCM)
	for i := 0; i < FrameSamples; i++ {
		v := int16(i * 200)
		src[2*i] = byte(v)
		src[2*i+1] = byte(v >> 8)
	}
	encoded := make([]byte, FrameBytesG711)
	decoded := make([]byte, FrameBytesPCM)

	n, err := EncodeUlaw(encoded, src)
	if err != nil {
		t.Fatal(err)
	}
	if n != FrameBytesG711 {
		t.Fatalf("encode: got %d bytes, want %d", n, FrameBytesG711)
	}

	n, err = DecodeUlaw(decoded, encoded)
	if err != nil {
		t.Fatal(err)
	}
	if n != FrameBytesPCM {
		t.Fatalf("decode: got %d bytes, want %d", n, FrameBytesPCM)
	}
	// G.711 is lossy; just verify the bytes changed and came back non-zero.
	allZero := true
	for _, b := range decoded {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("decoded output is all zeros")
	}
}

func TestAdapterFraming(t *testing.T) {
	a := NewAudioAdapter(nil)
	data := make([]byte, 500)
	for i := range data {
		data[i] = byte(i)
	}
	if _, err := a.Write(data); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, FrameBytesG711)
	n, err := a.Read(out)
	if err != nil {
		t.Fatal(err)
	}
	if n != FrameBytesG711 {
		t.Fatalf("got %d bytes, want %d", n, FrameBytesG711)
	}
}

func TestAdapterConcurrency(t *testing.T) {
	a := NewAudioAdapter(nil)
	const writers = 4
	const writesPerWriter = 20
	var wg sync.WaitGroup
	wg.Add(writers)
	for range writers {
		go func() {
			defer wg.Done()
			chunk := make([]byte, FrameBytesPCM)
			for range writesPerWriter {
				a.Write(chunk) //nolint:errcheck
			}
		}()
	}
	go func() {
		wg.Wait()
		a.Close()
	}()
	buf := make([]byte, FrameBytesPCM)
	for {
		_, err := a.Read(buf)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestAdapterClose(t *testing.T) {
	a := NewAudioAdapter(nil)
	go func() {
		a.Close()
	}()
	buf := make([]byte, FrameBytesPCM)
	_, err := a.Read(buf)
	if err != io.EOF {
		t.Fatalf("expected io.EOF after Close, got %v", err)
	}
}

func TestResampleOutputLength(t *testing.T) {
	src8 := make([]int16, FrameSamples)
	dst16 := make([]int16, FrameSamples*2)
	if err := Resample8to16(dst16, src8); err != nil {
		t.Fatal(err)
	}

	// 24kHz frame for 20ms = 480 samples
	src24 := make([]int16, 480)
	dst8 := make([]int16, 160)
	if err := Resample24to8(dst8, src24); err != nil {
		t.Fatal(err)
	}
}
