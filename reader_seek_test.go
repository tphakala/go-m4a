// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// seekTargets returns the sample indices worth seeking to in r: the first,
// second, middle and last unit, plus every chunk boundary and its neighbours,
// which is where an off-by-one in the chunk walk would land on the wrong unit.
func seekTargets(r *Reader) []int {
	n := r.sampleCount
	set := map[int]bool{0: true, 1: true, n / 2: true, n - 1: true}
	base := 0
	for _, c := range r.samplesPerChunk {
		base += int(c)
		for _, s := range []int{base - 1, base, base + 1} {
			set[s] = true
		}
	}
	out := make([]int, 0, len(set))
	for s := range set {
		if s >= 0 && s < n {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// assertSeekMatchesSequential seeks r to every target and checks that the
// following reads return exactly the units a sequential read returned from that
// point on, then a clean io.EOF. Every seek after the first starts from the end
// of the stream on the same Reader, so it also covers seeking back from io.EOF.
func assertSeekMatchesSequential(t *testing.T, r *Reader, want [][]byte) {
	t.Helper()
	for _, at := range seekTargets(r) {
		if err := r.SeekSample(at); err != nil {
			t.Fatalf("SeekSample(%d): %v", at, err)
		}
		for i := at; i < len(want); i++ {
			got, err := r.ReadFrame()
			if err != nil {
				t.Fatalf("SeekSample(%d): read unit %d: %v", at, i, err)
			}
			if !bytes.Equal(got, want[i]) {
				t.Fatalf("SeekSample(%d): unit %d differs from the sequential read", at, i)
			}
		}
		if _, err := r.ReadFrame(); !errors.Is(err, io.EOF) {
			t.Fatalf("SeekSample(%d): read past end = %v, want io.EOF", at, err)
		}
	}
}

// requireMultiChunk fails the test when r has a single chunk, so a fixture
// change cannot silently stop exercising the chunk walk.
func requireMultiChunk(t *testing.T, r *Reader) {
	t.Helper()
	if len(r.samplesPerChunk) < 2 {
		t.Fatalf("fixture has %d chunk(s), need at least 2 to exercise the chunk walk", len(r.samplesPerChunk))
	}
}

// TestSeekSampleMultiChunk seeks within afconvert files, whose stsc spreads the
// track over more than one chunk (the Writer always writes a single chunk).
func TestSeekSampleMultiChunk(t *testing.T) {
	for _, name := range []string{"afconvert_mono48k.m4a", "afconvert_stereo44k.m4a"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "interop", name))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			want := collectFrames(t, mustReader(t, data))
			r := mustReader(t, data)
			requireMultiChunk(t, r)
			assertSeekMatchesSequential(t, r, want)
		})
	}
}

// TestSeekSampleFragmented seeks across fragments of unequal length, each of
// which the reader lays out as one chunk.
func TestSeekSampleFragmented(t *testing.T) {
	frames := synthFrames(13)
	segs := [][]fragAU{
		uniformSegment(frames[0:3]),
		uniformSegment(frames[3:8]),
		uniformSegment(frames[8:9]),
		uniformSegment(frames[9:13]),
	}
	data := buildFragmentedStream(t, aacFragmentConfig(), segs)
	r := mustReader(t, data)
	requireMultiChunk(t, r)
	assertSeekMatchesSequential(t, r, frames)
}

// TestSeekSampleConstantSize covers the constant-size stsz path, where the
// offset within a chunk is a multiplication rather than a sum over sizes.
func TestSeekSampleConstantSize(t *testing.T) {
	const size = 40
	frames := make([][]byte, 9)
	for i := range frames {
		frames[i] = bytes.Repeat([]byte{byte(i + 1)}, size)
	}
	r := readerFromWriter(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, frames)
	// The Writer always emits per-sample sizes; switch this reader to the
	// constant-size form a third-party stsz may use, which is equivalent here.
	r.sizes, r.constSize = nil, size
	assertSeekMatchesSequential(t, r, frames)
}

// TestSeekSampleMidStream seeks after partial reads, both backwards and
// forwards, and checks the cursor fully replaces the earlier position.
func TestSeekSampleMidStream(t *testing.T) {
	frames := synthFrames(20)
	data := buildFragmentedStream(t, aacFragmentConfig(), [][]fragAU{
		uniformSegment(frames[0:7]), uniformSegment(frames[7:20]),
	})
	r := mustReader(t, data)
	for range 9 {
		if _, err := r.ReadFrame(); err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
	}
	for _, at := range []int{2, 15, 7, 6} {
		if err := r.SeekSample(at); err != nil {
			t.Fatalf("SeekSample(%d): %v", at, err)
		}
		got, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("ReadFrame after SeekSample(%d): %v", at, err)
		}
		if !bytes.Equal(got, frames[at]) {
			t.Errorf("SeekSample(%d) then ReadFrame returned the wrong unit", at)
		}
	}
}

// TestSeekSampleOutOfRange checks that an index outside the track wraps
// ErrOutOfRange and leaves the cursor where it was.
func TestSeekSampleOutOfRange(t *testing.T) {
	frames := synthFrames(5)
	r := readerFromWriter(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, frames)
	if err := r.SeekSample(3); err != nil {
		t.Fatalf("SeekSample(3): %v", err)
	}
	for _, bad := range []int{-1, 5, 1 << 30} {
		err := r.SeekSample(bad)
		if !errors.Is(err, ErrOutOfRange) {
			t.Errorf("SeekSample(%d) = %v, want ErrOutOfRange", bad, err)
		}
		if errors.Is(err, ErrCorrupt) {
			t.Errorf("SeekSample(%d) = %v, must not report the file as corrupt", bad, err)
		}
	}
	got, err := r.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame after rejected seeks: %v", err)
	}
	if !bytes.Equal(got, frames[3]) {
		t.Error("a rejected SeekSample moved the cursor")
	}
}

// TestSeekSampleRawStream checks the documented RawStream contract: a stream
// taken after the seek starts at the sought unit.
func TestSeekSampleRawStream(t *testing.T) {
	frames := synthFrames(6)
	r := readerFromWriter(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, frames)
	if err := r.SeekSample(4); err != nil {
		t.Fatalf("SeekSample(4): %v", err)
	}
	got, err := io.ReadAll(r.RawStream())
	if err != nil {
		t.Fatalf("read RawStream: %v", err)
	}
	var want []byte
	for _, f := range frames[4:] {
		want = append(want, byte(len(f)>>8), byte(len(f)))
		want = append(want, f...)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("RawStream after SeekSample(4) = %d bytes, want %d framed bytes of units 4 and 5", len(got), len(want))
	}
}

func mustReader(t *testing.T, data []byte) *Reader {
	t.Helper()
	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return r
}
