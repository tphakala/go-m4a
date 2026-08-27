// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// benchFrameCount is the number of AAC-LC access units each mux/demux benchmark
// processes. A few thousand frames is a realistic short clip (roughly a minute
// of 48 kHz audio at 1024 samples per frame) and is large enough that per-frame
// costs dominate the one-time setup and finalize work.
const benchFrameCount = 5000

// benchFrames builds n synthetic AAC-LC access units with realistic, varied
// sizes (roughly 100..800 bytes, the range of real AAC-LC frames). The bytes are
// arbitrary; the container never decodes them.
func benchFrames(n int) [][]byte {
	frames := make([][]byte, n)
	for i := range frames {
		size := 100 + (i*97)%701 // 100..800 bytes, varied
		au := make([]byte, size)
		for j := range au {
			au[j] = byte(i*31 + j)
		}
		frames[i] = au
	}
	return frames
}

// benchTotalPayload sums the access-unit lengths: the mdat payload byte count
// the mux and demux benchmarks move per iteration.
func benchTotalPayload(frames [][]byte) int64 {
	var total int64
	for _, f := range frames {
		total += int64(len(f))
	}
	return total
}

// buildBenchFile muxes frames into an in-memory M4A and returns the bytes.
func buildBenchFile(b *testing.B, frames [][]byte) []byte {
	b.Helper()
	ws := &memWS{}
	w, err := NewWriter(ws, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k})
	if err != nil {
		b.Fatal(err)
	}
	for _, au := range frames {
		if err := w.WriteFrame(au); err != nil {
			b.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}
	return ws.buf
}

// BenchmarkWriteFrames muxes benchFrameCount frames, including Close, into an
// in-memory io.WriteSeeker. The backing buffer is reused across iterations (only
// pos is reset) so the numbers isolate the Writer's own allocations from the
// sink's growth. Per-frame cost is (reported value / benchFrameCount).
func BenchmarkWriteFrames(b *testing.B) {
	frames := benchFrames(benchFrameCount)
	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}

	ws := &memWS{}
	// Warm-up mux grows the in-memory sink to the final file size. memWS.Write
	// reallocates and full-copies whenever the buffer must grow, so without this
	// the first measured iteration would charge an O(N^2) sink-growth cost to the
	// numbers. Pre-growing it makes later iterations reuse the backing buffer, so
	// the measured cost is the Writer's, not the test sink's. b.Loop excludes this
	// pre-loop work from the timer and allocation counters.
	muxOnce(b, ws, cfg, frames)

	b.SetBytes(benchTotalPayload(frames))
	b.ReportAllocs()
	for b.Loop() {
		ws.pos = 0 // reuse the backing buffer; measure only the Writer's allocs
		muxOnce(b, ws, cfg, frames)
	}
}

// muxOnce writes all frames to ws with a fresh Writer and closes it.
func muxOnce(b *testing.B, ws *memWS, cfg WriterConfig, frames [][]byte) {
	b.Helper()
	w, err := NewWriter(ws, cfg)
	if err != nil {
		b.Fatal(err)
	}
	for _, au := range frames {
		if err := w.WriteFrame(au); err != nil {
			b.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		b.Fatal(err)
	}
}

// BenchmarkReadFrame demuxes every frame via ReadFrame. The Reader is parsed
// once; each iteration only resets the cursor, so the numbers reflect the
// steady-state per-frame read cost (one make([]byte, size) per frame is the
// documented ReadFrame contract). Per-frame cost is (value / benchFrameCount).
func BenchmarkReadFrame(b *testing.B) {
	frames := benchFrames(benchFrameCount)
	data := buildBenchFile(b, frames)

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(benchTotalPayload(frames))
	b.ReportAllocs()
	for b.Loop() {
		rd.resetCursor()
		for {
			_, rerr := rd.ReadFrame()
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				b.Fatal(rerr)
			}
		}
	}
}

// BenchmarkReadFrameInto demuxes every frame via ReadFrameInto into a single
// reused buffer sized to the largest frame. The Reader is parsed once and each
// iteration only resets the cursor, so the numbers reflect the steady-state
// per-frame read cost: zero allocations, since the buffer is reused rather than
// allocated per frame (unlike ReadFrame's documented one make per frame). Per-
// frame cost is (value / benchFrameCount).
func BenchmarkReadFrameInto(b *testing.B) {
	frames := benchFrames(benchFrameCount)
	data := buildBenchFile(b, frames)

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		b.Fatal(err)
	}

	// Size the reused buffer to the largest frame so ReadFrameInto never returns
	// io.ErrShortBuffer in the measured loop.
	var maxSize int
	for _, f := range frames {
		if len(f) > maxSize {
			maxSize = len(f)
		}
	}
	dst := make([]byte, maxSize)

	b.SetBytes(benchTotalPayload(frames))
	b.ReportAllocs()
	for b.Loop() {
		rd.resetCursor()
		for {
			_, rerr := rd.ReadFrameInto(dst)
			if errors.Is(rerr, io.EOF) {
				break
			}
			if rerr != nil {
				b.Fatal(rerr)
			}
		}
	}
}

// BenchmarkRawStream drives the primary go-aac decode feed: io.Copy from a
// RawStream over the built file into io.Discard. The rawReader is reused with a
// pre-warmed scratch buffer so the numbers show the steady-state per-frame cost,
// which is expected to be zero allocations after warm-up.
func BenchmarkRawStream(b *testing.B) {
	frames := benchFrames(benchFrameCount)
	data := buildBenchFile(b, frames)
	framed := benchTotalPayload(frames) + 2*int64(len(frames)) // + 2-byte length prefixes

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		b.Fatal(err)
	}
	rr := &rawReader{rd: rd}
	// Warm the scratch buffer to the largest frame so its one-time growth is not
	// charged to the measured loop.
	rd.resetCursor()
	if _, err := io.Copy(io.Discard, rr); err != nil {
		b.Fatal(err)
	}

	b.SetBytes(framed)
	b.ReportAllocs()
	for b.Loop() {
		rd.resetCursor()
		rr.buf = nil
		rr.err = nil
		if _, err := io.Copy(io.Discard, rr); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRawStreamFresh is the realistic-usage variant: a fresh RawStream per
// iteration (as aacm4a.NewDecoder does once per file). It charges the one-time
// scratch growth to every iteration, so allocs/op reflect that growth amortized
// over all frames rather than the true per-frame cost.
func BenchmarkRawStreamFresh(b *testing.B) {
	frames := benchFrames(benchFrameCount)
	data := buildBenchFile(b, frames)
	framed := benchTotalPayload(frames) + 2*int64(len(frames))

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(framed)
	b.ReportAllocs()
	for b.Loop() {
		rd.resetCursor()
		if _, err := io.Copy(io.Discard, rd.RawStream()); err != nil {
			b.Fatal(err)
		}
	}
}

// benchFragSegments splits frames into fixed-size media segments, recording a
// constant per-sample duration, so buildFragmentedStream emits one moof per
// segment. framesPerSeg controls the moof count the demux setup must walk.
func benchFragSegments(frames [][]byte, framesPerSeg int) [][]fragAU {
	segments := make([][]fragAU, 0, (len(frames)+framesPerSeg-1)/framesPerSeg)
	for i := 0; i < len(frames); i += framesPerSeg {
		end := min(i+framesPerSeg, len(frames))
		seg := make([]fragAU, 0, end-i)
		for _, au := range frames[i:end] {
			seg = append(seg, fragAU{au: au, dur: 1024}) // 1024 samples per AAC-LC frame
		}
		segments = append(segments, seg)
	}
	return segments
}

// BenchmarkOpenFragmented measures the one-time fragmented (CMAF) demux setup:
// scanTopLevel plus buildFragmentGeometry walking every moof to lay out the
// sample geometry. The stream is built once outside the timer; each iteration
// runs NewReader over it. This is the baseline a size-only trun fast path
// (tracked in #44) would have to beat, so it is measured with ReportAllocs. No
// SetBytes: the demux setup reads only the box headers and moof bodies, never
// the mdat media payload, so a bytes-per-second figure would misreport it.
func BenchmarkOpenFragmented(b *testing.B) {
	frames := benchFrames(benchFrameCount)
	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
	data := buildFragmentedStream(b, cfg, benchFragSegments(frames, 50))

	b.ReportAllocs()
	for b.Loop() {
		if _, err := NewReader(bytes.NewReader(data)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBuildMoov measures Close's moov assembly for a large frame count,
// exercising the AppendContainer re-copies that carry the sample table up the
// box tree (stbl -> minf -> mdia -> trak -> moov).
func BenchmarkBuildMoov(b *testing.B) {
	frames := benchFrames(benchFrameCount)
	sizes := make([]uint32, len(frames))
	for i, f := range frames {
		sizes[i] = uint32(len(f))
	}
	w := &Writer{
		trackMeta: trackMeta{
			sampleRate: 48000,
			channels:   1,
			asc:        ascMono48k,
		},
		payloadStart: 100,
		sizes:        sizes,
	}
	b.ReportAllocs()
	for b.Loop() {
		_ = w.buildMoov()
	}
}
