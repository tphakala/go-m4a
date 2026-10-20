// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// countingReadSeeker counts the Seek and Read calls that reach the source and
// the bytes the Reads return. A positive maxRead caps the bytes each Read returns, as a network source that
// delivers one chunk per Read would.
type countingReadSeeker struct {
	io.ReadSeeker

	maxRead      int
	seeks, reads int
	read         int64
}

func (c *countingReadSeeker) Seek(off int64, whence int) (int64, error) {
	c.seeks++
	return c.ReadSeeker.Seek(off, whence)
}

func (c *countingReadSeeker) Read(p []byte) (int, error) {
	c.reads++
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
					rd.win = make([]byte, 0, w)
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
			if got := cap(rd.win); got != 64<<10 {
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
	rd.win = make([]byte, 0, 64)
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
	rd.win = make([]byte, 0, 64)
	src.read = 0
	drainMatchesStream(t, rd, data, nil)
	if want := int64(len(frames) * 40); src.read != want {
		t.Errorf("reading %d frames of 40 bytes through a 64-byte window read %d bytes from the source, want %d", len(frames), src.read, want)
	}
}
