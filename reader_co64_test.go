// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"testing"

	"github.com/tphakala/go-m4a/internal/box"
)

// handAssembleM4A builds a minimal but fully valid ftyp|mdat|moov file, modeled
// on writer.buildMoov, but with the sample-size and chunk-offset tables supplied
// by the caller. It lets the reader tests exercise table variants the Writer
// never emits (co64 offsets, a constant-size stsz). payload is the raw mdat
// payload; the file uses a single chunk, so the caller's offsetBox must encode
// exactly one offset, which it receives as payloadStart. The moov is placed after
// mdat, exactly like the Writer. This helper is shared by reader_conststsz_test.go.
func handAssembleM4A(sampleRate uint32, channels uint16, asc []byte, n int, payload, stsz []byte, offsetBox func(payloadStart int64) []byte) []byte {
	ftyp := box.AppendFtyp(nil, box.NewFourCC("M4A "), 0,
		box.NewFourCC("M4A "), box.NewFourCC("mp42"), box.NewFourCC("isom"))

	// The single chunk begins right after the 16-byte largesize mdat header, at
	// the same payloadStart the Writer computes.
	payloadStart := int64(len(ftyp) + box.MdatHeaderSize)
	mdat := box.AppendLargeBoxHeader(nil, uint64(box.MdatHeaderSize+len(payload)), box.NewFourCC("mdat"))
	mdat = append(mdat, payload...)

	// stbl: sample description, timing, sample-to-chunk (one chunk of N samples),
	// then the caller's size and chunk-offset tables.
	var stbl []byte
	stbl = box.AppendStsd(stbl, channels, sampleRate, asc)
	stbl = box.AppendStts(stbl, uint32(n), samplesPerFrame)
	stbl = box.AppendStsc(stbl, 1, uint32(n), 1)
	stbl = append(stbl, stsz...)
	stbl = append(stbl, offsetBox(payloadStart)...)

	// minf: sound media header, self-contained data reference, sample table.
	var minf []byte
	minf = box.AppendSmhd(minf)
	minf = box.AppendDinf(minf)
	minf = box.AppendStbl(minf, stbl)

	// mdia: media header, sound handler, media information.
	mediaDuration := uint64(n) * samplesPerFrame
	var mdia []byte
	mdia = box.AppendMdhd(mdia, sampleRate, mediaDuration)
	mdia = box.AppendHdlr(mdia, box.NewFourCC("soun"), "SoundHandler")
	mdia = box.AppendMinf(mdia, minf)

	// trak: track header then media. No edit list is needed for these tests.
	var trak []byte
	trak = box.AppendTkhd(trak, 1, mediaDuration)
	trak = box.AppendMdia(trak, mdia)

	// moov: movie header then the single track.
	moov := box.AppendMvhd(nil, sampleRate, mediaDuration)
	moov = box.AppendTrak(moov, trak)
	moov = box.AppendMoov(nil, moov)

	file := make([]byte, 0, len(ftyp)+len(mdat)+len(moov))
	file = append(file, ftyp...)
	file = append(file, mdat...)
	return append(file, moov...)
}

// TestReaderCo64 covers the co64 (64-bit chunk offset) reader path. The Writer
// always emits stco, so a hand-assembled file is the only way to reach ParseCo64
// and the co64 branch of chunkOffsetTable. The file carries no stco, so a correct
// read proves the 64-bit offset table was the one used.
func TestReaderCo64(t *testing.T) {
	const n = 5
	frames := synthFrames(n)

	sizes := make([]uint32, n)
	var payload []byte
	for i, f := range frames {
		sizes[i] = uint32(len(f))
		payload = append(payload, f...)
	}

	stsz := box.AppendStsz(nil, sizes)
	offsetBox := func(payloadStart int64) []byte {
		// A single 64-bit offset, encoded as an 8-byte co64 entry.
		return box.AppendCo64(nil, []uint64{uint64(payloadStart)})
	}

	data := handAssembleM4A(48000, 1, ascMono48k, n, payload, stsz, offsetBox)

	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	info := r.Info()
	if info.SampleRate != 48000 {
		t.Errorf("SampleRate = %d, want 48000", info.SampleRate)
	}
	if info.Channels != 1 {
		t.Errorf("Channels = %d, want 1", info.Channels)
	}
	if info.FrameCount != n {
		t.Errorf("FrameCount = %d, want %d", info.FrameCount, n)
	}
	if !bytes.Equal(info.ASC, ascMono48k) {
		t.Errorf("ASC = % x, want % x", info.ASC, ascMono48k)
	}

	got := collectFrames(t, r)
	if len(got) != n {
		t.Fatalf("read %d frames, want %d", len(got), n)
	}
	for i := range frames {
		if !bytes.Equal(got[i], frames[i]) {
			t.Errorf("co64 frame %d = % x, want % x", i, got[i], frames[i])
		}
	}
}

// TestReaderCo64Stereo exercises the co64 path once more with the stereo 44.1 kHz
// format, confirming the 64-bit offset table does not disturb format resolution.
func TestReaderCo64Stereo(t *testing.T) {
	const n = 4
	frames := synthFrames(n)

	sizes := make([]uint32, n)
	var payload []byte
	for i, f := range frames {
		sizes[i] = uint32(len(f))
		payload = append(payload, f...)
	}

	stsz := box.AppendStsz(nil, sizes)
	offsetBox := func(payloadStart int64) []byte {
		return box.AppendCo64(nil, []uint64{uint64(payloadStart)})
	}

	data := handAssembleM4A(44100, 2, ascStereo44k, n, payload, stsz, offsetBox)

	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	info := r.Info()
	if info.SampleRate != 44100 || info.Channels != 2 {
		t.Errorf("format = %d Hz / %d ch, want 44100 / 2", info.SampleRate, info.Channels)
	}
	if info.FrameCount != n {
		t.Errorf("FrameCount = %d, want %d", info.FrameCount, n)
	}
	got := collectFrames(t, r)
	if len(got) != n {
		t.Fatalf("read %d frames, want %d", len(got), n)
	}
	for i := range frames {
		if !bytes.Equal(got[i], frames[i]) {
			t.Errorf("co64 stereo frame %d mismatch", i)
		}
	}
}
