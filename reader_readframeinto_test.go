// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// TestReadFrameIntoRoundTrip writes N frames of varied sizes, then reads them
// all back through ReadFrameInto reusing a single buffer sized to the largest
// frame. Every returned length and byte slice must match what ReadFrame yields
// for the same file, and io.EOF must arrive only after the Nth frame.
func TestReadFrameIntoRoundTrip(t *testing.T) {
	const n = 200
	frames := synthFrames(n)
	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
	data := writeM4A(t, cfg, frames)

	// Reference decode: collect every frame via ReadFrame.
	ref, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader (reference): %v", err)
	}
	var want [][]byte
	for {
		au, rerr := ref.ReadFrame()
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			t.Fatalf("ReadFrame: %v", rerr)
		}
		want = append(want, au)
	}
	if len(want) != n {
		t.Fatalf("ReadFrame returned %d frames, want %d", len(want), n)
	}

	// Size the reused buffer to the largest frame.
	maxSize := 0
	for _, f := range want {
		if len(f) > maxSize {
			maxSize = len(f)
		}
	}
	dst := make([]byte, maxSize)

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	for i := 0; i < n; i++ {
		got, rerr := rd.ReadFrameInto(dst)
		if rerr != nil {
			t.Fatalf("ReadFrameInto frame %d: %v", i, rerr)
		}
		if got != len(want[i]) {
			t.Fatalf("ReadFrameInto frame %d: length %d, want %d", i, got, len(want[i]))
		}
		if !bytes.Equal(dst[:got], want[i]) {
			t.Fatalf("ReadFrameInto frame %d: bytes differ from ReadFrame", i)
		}
	}
	// io.EOF only after the last access unit.
	if got, rerr := rd.ReadFrameInto(dst); !errors.Is(rerr, io.EOF) {
		t.Fatalf("ReadFrameInto after %d frames: got (%d, %v), want (0, io.EOF)", n, got, rerr)
	}
}

// TestReadFrameIntoShortBuffer verifies the short-buffer retry contract: a dst
// too small returns the required length with io.ErrShortBuffer, reads nothing,
// and does not advance the cursor, so a retry with an adequate buffer returns
// that very same frame.
func TestReadFrameIntoShortBuffer(t *testing.T) {
	frames := synthFrames(8)
	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
	data := writeM4A(t, cfg, frames)

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	wantSize := len(frames[0])
	if wantSize <= 1 {
		t.Fatalf("test setup: first frame is %d bytes, need > 1", wantSize)
	}

	// A one-byte destination cannot hold the frame: expect (frameSize, ErrShortBuffer).
	tiny := make([]byte, 1)
	got, rerr := rd.ReadFrameInto(tiny)
	if !errors.Is(rerr, io.ErrShortBuffer) {
		t.Fatalf("ReadFrameInto(small): error %v, want io.ErrShortBuffer", rerr)
	}
	if got != wantSize {
		t.Fatalf("ReadFrameInto(small): required length %d, want %d", got, wantSize)
	}

	// The cursor must not have advanced: a retry with an adequate buffer returns
	// the same (first) frame, proving no bytes were consumed.
	big := make([]byte, wantSize)
	got, rerr = rd.ReadFrameInto(big)
	if rerr != nil {
		t.Fatalf("ReadFrameInto(big) retry: %v", rerr)
	}
	if got != wantSize {
		t.Fatalf("ReadFrameInto(big) retry: length %d, want %d", got, wantSize)
	}
	if !bytes.Equal(big[:got], frames[0]) {
		t.Fatalf("ReadFrameInto(big) retry: bytes differ from the first written frame")
	}
}
