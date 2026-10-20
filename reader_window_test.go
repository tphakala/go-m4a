// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// countingReadSeeker counts the Seek and Read calls that reach the source and
// the bytes the Reads return. A positive maxRead caps the bytes each Read returns, as a network source that
// delivers one chunk per Read would. maxReq is the largest buffer any Read was
// offered, before that cap.
type countingReadSeeker struct {
	io.ReadSeeker

	maxRead      int
	seeks, reads int
	read         int64
	maxReq       int
}

// openWindow overrides NewReader's open-time window size; 0 keeps the default.
// Size 1 makes every open-time read direct, the unbuffered reference the
// windowed reads must match.
func openWindow(n int) ReaderOption {
	return func(c *readerConfig) {
		if n > 0 {
			c.openWindow = n
		}
	}
}

func (c *countingReadSeeker) Seek(off int64, whence int) (int64, error) {
	c.seeks++
	return c.ReadSeeker.Seek(off, whence)
}

func (c *countingReadSeeker) Read(p []byte) (int, error) {
	c.reads++
	c.maxReq = max(c.maxReq, len(p))
	if c.maxRead > 0 && len(p) > c.maxRead {
		p = p[:c.maxRead]
	}
	n, err := c.ReadSeeker.Read(p)
	c.read += int64(n)
	return n, err
}

// drainMatchesStream reads every remaining frame of rd and checks each against
// the input bytes at the offset the cursor named, then a clean io.EOF. The input
// is the ground truth: comparing against frames read through the same Reader
// would let a consistent window bug cancel out. A non-nil between runs after
// each frame.
func drainMatchesStream(t *testing.T, rd *Reader, data []byte, between func()) {
	t.Helper()
	for {
		off := rd.curOffset
		sample := rd.nextSample
		frame, err := rd.ReadFrame()
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			t.Fatalf("frame %d: %v", sample, err)
		}
		if !bytes.Equal(frame, data[off:off+int64(len(frame))]) {
			t.Fatalf("frame %d at offset %d differs from the stream bytes", sample, off)
		}
		if between != nil {
			between()
		}
	}
}

// TestFrameReadsMatchStreamBytes sweeps the read-ahead window size against
// fixtures with one chunk, several fragments (chunk jumps over moof headers) and
// several stsc chunks, starting at every seek target. Small windows force
// straddling frames, frames read directly, and backward seeks into or out of
// the window; every frame must still be the stream's bytes at its offset.
func TestFrameReadsMatchStreamBytes(t *testing.T) {
	frames := synthFrames(13)
	fixtures := map[string][]byte{
		"single chunk": writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, synthFrames(40)),
		"fragmented": buildFragmentedStream(t, aacFragmentConfig(), [][]fragAU{
			uniformSegment(frames[0:3]),
			uniformSegment(frames[3:8]),
			uniformSegment(frames[8:9]),
			uniformSegment(frames[9:13]),
		}),
	}
	afconvert, err := os.ReadFile(filepath.Join("testdata", "interop", "afconvert_mono48k.m4a"))
	if err != nil {
		t.Fatal(err)
	}
	fixtures["multi chunk"] = afconvert

	for name, data := range fixtures {
		t.Run(name, func(t *testing.T) {
			// Window 0 keeps the default size; the rest preset a small one.
			for w := range 401 {
				rd := mustReader(t, data)
				if w > 0 {
					rd.win.buf = make([]byte, 0, w)
				}
				for _, start := range seekTargets(rd) {
					if err := rd.SeekSample(start); err != nil {
						t.Fatalf("window %d: SeekSample(%d): %v", w, start, err)
					}
					drainMatchesStream(t, rd, data, nil)
				}
			}
		})
	}
}

// TestForwardDecodeReadsInWindows pins the point of the read-ahead window: a
// forward decode seeks and reads the source once per window, not once per
// access unit, and refills reuse the window without allocating.
func TestForwardDecodeReadsInWindows(t *testing.T) {
	frames := benchFrames()[:300]
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, frames)
	dst := make([]byte, maxFrameLen(frames))

	drain := func(t *testing.T, rd *Reader) {
		t.Helper()
		if err := rd.SeekSample(0); err != nil {
			t.Fatal(err)
		}
		for {
			_, err := rd.ReadFrameInto(dst)
			if errors.Is(err, io.EOF) {
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}

	for _, tc := range []struct {
		name    string
		maxRead int
	}{
		{name: "whole reads"},
		// A source that returns 4 KiB per Read still costs one Seek per window,
		// so a range-request source does not issue a request per chunk.
		{name: "short reads", maxRead: 4 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &countingReadSeeker{ReadSeeker: bytes.NewReader(data), maxRead: tc.maxRead}
			rd, err := NewReader(src)
			if err != nil {
				t.Fatal(err)
			}
			src.seeks, src.reads = 0, 0
			drain(t, rd)

			span := rd.streamLen - rd.chunkOffsets[0]
			limit := int((span+frameWindowSize-1)/frameWindowSize) + 1
			if src.seeks > limit {
				t.Errorf("forward decode of %d frames made %d Seek calls, want at most %d", len(frames), src.seeks, limit)
			}
			// The stream is longer than the window, so the window takes its full
			// documented size, never one derived from the stream.
			if got := cap(rd.win.buf); got != 64<<10 {
				t.Errorf("read-ahead window holds %d bytes over a %d-byte stream, want 65536", got, rd.streamLen)
			}
			if tc.maxRead > 0 {
				return
			}
			if src.reads > limit {
				t.Errorf("forward decode of %d frames made %d Read calls, want at most %d", len(frames), src.reads, limit)
			}
			// Each drain refills the window several times; none may allocate.
			if allocs := testing.AllocsPerRun(5, func() { drain(t, rd) }); allocs != 0 {
				t.Errorf("forward decode allocated %.0f times per run after warm-up, want 0", allocs)
			}
		})
	}
}

// TestFrameReadIgnoresStreamPosition moves the shared source between frame
// reads, as a caller holding the same io.ReadSeeker may. Every refill seeks
// first, so the frames must not depend on where the stream was left. Four
// 16-byte frames fill each 64-byte window exactly, so every refill starts where
// the previous read from the source ended: the case a refill that skipped its
// Seek would get wrong.
func TestFrameReadIgnoresStreamPosition(t *testing.T) {
	frames := make([][]byte, 40)
	for i := range frames {
		frames[i] = bytes.Repeat([]byte{byte(i + 1)}, 16)
	}
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, frames)
	src := bytes.NewReader(data)
	rd, err := NewReader(src)
	if err != nil {
		t.Fatal(err)
	}
	rd.win.buf = make([]byte, 0, 64)
	var scratch [16]byte
	drainMatchesStream(t, rd, data, func() {
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(src, scratch[:]); err != nil {
			t.Fatal(err)
		}
	})
}

// TestLargeFramesReadDirectly pins that a frame larger than half the window is
// read straight into the caller's buffer. Refilling for it would read a whole
// window to serve at most one more frame, so the source would return more bytes
// than the frames hold.
func TestLargeFramesReadDirectly(t *testing.T) {
	frames := make([][]byte, 20)
	for i := range frames {
		frames[i] = bytes.Repeat([]byte{byte(i + 1)}, 40)
	}
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, frames)
	src := &countingReadSeeker{ReadSeeker: bytes.NewReader(data)}
	rd, err := NewReader(src)
	if err != nil {
		t.Fatal(err)
	}
	rd.win.buf = make([]byte, 0, 64)
	src.read = 0
	drainMatchesStream(t, rd, data, nil)
	if want := int64(len(frames) * 40); src.read != want {
		t.Errorf("reading %d frames of 40 bytes through a 64-byte window read %d bytes from the source, want %d", len(frames), src.read, want)
	}
}

// TestReadAheadWindowFitsShortStream pins that the frame window never exceeds
// the stream it reads: a 2870-byte file gets a 2870-byte window, not the 64 KiB
// a long stream gets (TestForwardDecodeReadsInWindows pins that side).
func TestReadAheadWindowFitsShortStream(t *testing.T) {
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, synthFrames(40))
	rd := mustReader(t, data)
	if _, err := rd.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if got := cap(rd.win.buf); got != len(data) {
		t.Errorf("read-ahead window holds %d bytes over a %d-byte stream, want %d", got, len(data), len(data))
	}
}

// TestOpenReadsThroughWindow pins that NewReader reads box headers and bodies
// through its open window: a refill serves a segment's styp, moof and mdat header
// together, so opening N segments costs about two Seek and Read pairs each rather
// than one per box, and no Read is offered more than the window.
func TestOpenReadsThroughWindow(t *testing.T) {
	t.Run("fragmented", func(t *testing.T) {
		const segments = 30
		frames := benchFrames()
		segs := make([][]fragAU, segments)
		for i := range segs {
			segs[i] = uniformSegment(frames[16*i : 16*i+16])
		}
		data := buildFragmentedStream(t, aacFragmentConfig(), segs)

		src := &countingReadSeeker{ReadSeeker: bytes.NewReader(data)}
		if _, err := NewReader(src); err != nil {
			t.Fatal(err)
		}
		if limit := 2*segments + 2; src.seeks > limit {
			t.Errorf("opening %d segments made %d Seek calls, want at most %d", segments, src.seeks, limit)
		}
		if limit := 2*segments + 1; src.reads > limit {
			t.Errorf("opening %d segments made %d Read calls, want at most %d", segments, src.reads, limit)
		}
		if src.maxReq > openWindowSize {
			t.Errorf("opening a %d-byte stream offered a Read %d bytes, want at most the %d-byte open window", len(data), src.maxReq, openWindowSize)
		}
	})

	// ftyp and moov occur once per file, so only a plain file shows whether
	// readSection goes through the window: the 2870-byte file fits one refill.
	t.Run("plain", func(t *testing.T) {
		data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, synthFrames(40))
		src := &countingReadSeeker{ReadSeeker: bytes.NewReader(data)}
		if _, err := NewReader(src); err != nil {
			t.Fatal(err)
		}
		if src.reads != 1 {
			t.Errorf("opening a %d-byte plain file made %d Read calls, want 1", len(data), src.reads)
		}
	})
}

// openOutcome opens src and renders everything NewReader decided: the error text
// and which sentinels it matches, or the Info and the sample geometry.
func openOutcome(src io.ReadSeeker, opts ...ReaderOption) string {
	rd, err := NewReader(src, opts...)
	if err != nil {
		return fmt.Sprintf("error %q corrupt=%t unsupported=%t tooLarge=%t eof=%t unexpectedEOF=%t", err,
			errors.Is(err, ErrCorrupt), errors.Is(err, ErrUnsupported), errors.Is(err, ErrBoxTooLarge),
			errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF))
	}
	return fmt.Sprintf("%#v|%v|%d|%v|%v|%v|%#v", rd.Info(), rd.sizes, rd.constSize, rd.chunkOffsets,
		rd.samplesPerChunk, rd.timeRuns, rd.window)
}

// TestOpenOutcomeIndependentOfWindow pins that the open window changes how
// NewReader reads, never what it decides: for any cut of the stream and any
// window size the error (text and wrapped sentinels) or the resulting geometry
// equals what unbuffered reads (window size 1) give.
func TestOpenOutcomeIndependentOfWindow(t *testing.T) {
	frames := synthFrames(13)
	afconvert, err := os.ReadFile(filepath.Join("testdata", "interop", "afconvert_mono48k.m4a"))
	if err != nil {
		t.Fatal(err)
	}
	fixtures := []struct {
		name string
		data []byte
	}{
		{"plain", writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, synthFrames(40))},
		{"fragmented", buildFragmentedStream(t, aacFragmentConfig(), [][]fragAU{
			uniformSegment(frames[0:3]),
			uniformSegment(frames[3:8]),
			uniformSegment(frames[8:9]),
			uniformSegment(frames[9:13]),
		})},
		{"multi chunk", afconvert},
	}

	for _, fx := range fixtures {
		t.Run(fx.name, func(t *testing.T) {
			// Cut the stream at every length while the source still claims the full
			// one, so each box header and body meets a source that ends early.
			for k := 0; k <= len(fx.data); k++ {
				open := func(w int) string {
					return openOutcome(&shortSource{Reader: bytes.NewReader(fx.data[:k]), claim: int64(len(fx.data))}, openWindow(w))
				}
				want := open(1)
				for _, w := range []int{0, 64, 40, 33} {
					if got := open(w); got != want {
						t.Fatalf("cut at %d, window %d: got %s, want %s", k, w, got, want)
					}
				}
			}
			want := openOutcome(bytes.NewReader(fx.data), openWindow(1))
			for w := 2; w <= 300; w++ {
				if got := openOutcome(bytes.NewReader(fx.data), openWindow(w)); got != want {
					t.Fatalf("whole stream, window %d: got %s, want %s", w, got, want)
				}
			}
		})
	}
}
