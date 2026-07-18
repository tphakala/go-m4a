// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/tphakala/go-m4a/internal/box"
)

// assembleMoovStbl builds a complete ftyp|mdat|moov file whose stbl body is
// produced by buildStbl, given the file's single-chunk payloadStart. It mirrors
// handAssembleM4A for every box outside the sample table, so a test can inject a
// malformed or multi-chunk stsc/stsz/stts/stco and drive the result through
// NewReader. n sets the mvhd/mdhd media duration (n * 1024 samples); it does not
// have to agree with the sample tables, which is exactly what the malformed
// cases need.
func assembleMoovStbl(sampleRate uint32, channels uint16, n int, payload []byte, buildStbl func(payloadStart int64) []byte) []byte {
	ftyp := box.AppendFtyp(nil, box.NewFourCC("M4A "), 0,
		box.NewFourCC("M4A "), box.NewFourCC("mp42"), box.NewFourCC("isom"))

	payloadStart := int64(len(ftyp) + box.MdatHeaderSize)
	mdat := box.AppendLargeBoxHeader(nil, uint64(box.MdatHeaderSize+len(payload)), box.NewFourCC("mdat"))
	mdat = append(mdat, payload...)

	stbl := buildStbl(payloadStart)

	var minf []byte
	minf = box.AppendSmhd(minf)
	minf = box.AppendDinf(minf)
	minf = box.AppendStbl(minf, stbl)

	mediaDuration := uint64(n) * samplesPerFrame
	var mdia []byte
	mdia = box.AppendMdhd(mdia, sampleRate, mediaDuration)
	mdia = box.AppendHdlr(mdia, box.NewFourCC("soun"), "SoundHandler")
	mdia = box.AppendMinf(mdia, minf)

	var trak []byte
	trak = box.AppendTkhd(trak, 1, mediaDuration)
	trak = box.AppendMdia(trak, mdia)

	moov := box.AppendMvhd(nil, sampleRate, mediaDuration)
	moov = box.AppendTrak(moov, trak)
	moov = box.AppendMoov(nil, moov)

	file := make([]byte, 0, len(ftyp)+len(mdat)+len(moov))
	file = append(file, ftyp...)
	file = append(file, mdat...)
	return append(file, moov...)
}

// joinStbl concatenates the sample-table box bytes into an stbl body. Omitting a
// part (passing nil) is how the negative cases drop a required box.
func joinStbl(parts ...[]byte) []byte {
	var b []byte
	for _, p := range parts {
		b = append(b, p...)
	}
	return b
}

// stscBox marshals an stsc FullBox with the given run entries. box.AppendStsc
// only emits a single entry, so multi-entry and deliberately malformed tables are
// assembled here. The FullBox is header(8) + version/flags(4) + entry_count(4) +
// 12 bytes per entry.
func stscBox(entries ...box.StscEntry) []byte {
	size := uint32(16 + 12*len(entries))
	b := box.AppendFullBoxHeader(nil, size, box.NewFourCC("stsc"), 0, 0)
	b = binary.BigEndian.AppendUint32(b, uint32(len(entries)))
	for _, e := range entries {
		b = binary.BigEndian.AppendUint32(b, e.FirstChunk)
		b = binary.BigEndian.AppendUint32(b, e.SamplesPerChunk)
		b = binary.BigEndian.AppendUint32(b, e.SampleDescriptionIndex)
	}
	return b
}

// objectTypeOffsetInMp4a is the byte offset of the esds objectTypeIndication
// within an mp4a box produced by box.AppendMp4a: header(8) + fixed
// AudioSampleEntry(28) + esds header(8) + version/flags(4) + ES tag/len(2) +
// ES_ID/flags(3) + DecoderConfigDescriptor tag/len(2) = 55. It is fixed
// regardless of the ASC length, since the ASC follows it.
const objectTypeOffsetInMp4a = 55

// stsdObjType builds an stsd whose mp4a esds carries a chosen objectTypeIndication
// byte, so a test can present a non-AAC object type (anything but 0x40).
func stsdObjType(t *testing.T, channels uint16, sampleRate uint32, asc []byte, objType byte) []byte {
	t.Helper()
	mp4a := box.AppendMp4a(nil, channels, sampleRate, asc)
	if mp4a[objectTypeOffsetInMp4a] != 0x40 {
		t.Fatalf("mp4a layout drift: objectType byte at %d is %#x, want 0x40", objectTypeOffsetInMp4a, mp4a[objectTypeOffsetInMp4a])
	}
	mp4a[objectTypeOffsetInMp4a] = objType
	return wrapStsd(mp4a)
}

// stsdCodec builds an stsd whose single sample entry is a minimal box of the
// given four-character codec (not mp4a), so a test can present a soun track with
// an unsupported codec.
func stsdCodec(codec string) []byte {
	se := box.AppendBoxHeader(nil, 8, box.NewFourCC(codec)) // 8-byte sample entry, empty body
	return wrapStsd(se)
}

// wrapStsd wraps a single sample entry into an stsd FullBox with entry_count 1.
func wrapStsd(sampleEntry []byte) []byte {
	size := uint32(12 + 4 + len(sampleEntry)) // FullBox header(12) + entry_count(4) + entry
	b := box.AppendFullBoxHeader(nil, size, box.NewFourCC("stsd"), 0, 0)
	b = binary.BigEndian.AppendUint32(b, 1) // entry_count
	return append(b, sampleEntry...)
}

// framePayload concatenates frames into one contiguous mdat payload and returns
// it together with the per-sample size table.
func framePayload(frames [][]byte) (payload []byte, sizes []uint32) {
	sizes = make([]uint32, len(frames))
	for i, f := range frames {
		sizes[i] = uint32(len(f))
		payload = append(payload, f...)
	}
	return payload, sizes
}

// TestExpandStscRegression locks in the fix for the expandStsc out-of-bounds
// panic. Each case hand-assembles a structurally valid moov whose stsc drives the
// per-chunk fill loop out of range on the pre-fix code (an unchecked next
// first_chunk of 0 underflowed to 0xFFFFFFFF, or a value past numChunks widened
// the run). NewReader must reject every one as a wrapped ErrCorrupt and must not
// panic.
func TestExpandStscRegression(t *testing.T) {
	stsd := box.AppendStsd(nil, 1, 48000, ascMono48k)

	tests := []struct {
		name    string
		n       int
		payload []byte
		build   func(payloadStart int64) []byte
	}{
		{
			// entry1.first_chunk == 0: the classic panic shape. numChunks 1,
			// sample_count 0, second run first_chunk 0 underflows to 0xFFFFFFFF.
			name:    "next first_chunk zero underflow",
			n:       0,
			payload: nil,
			build: func(ps int64) []byte {
				return joinStbl(
					stsd,
					stscBox(
						box.StscEntry{FirstChunk: 1, SamplesPerChunk: 0, SampleDescriptionIndex: 1},
						box.StscEntry{FirstChunk: 0, SamplesPerChunk: 0, SampleDescriptionIndex: 1},
					),
					box.AppendStsz(nil, nil), // sample_count 0
					box.AppendStco(nil, []uint32{uint32(ps)}),
				)
			},
		},
		{
			// entry1.first_chunk past numChunks: last = next-1 widens the run past
			// the perChunk slice on the pre-fix code.
			name:    "next first_chunk past numChunks",
			n:       0,
			payload: nil,
			build: func(ps int64) []byte {
				return joinStbl(
					stsd,
					stscBox(
						box.StscEntry{FirstChunk: 1, SamplesPerChunk: 0, SampleDescriptionIndex: 1},
						box.StscEntry{FirstChunk: 5, SamplesPerChunk: 0, SampleDescriptionIndex: 1},
					),
					box.AppendStsz(nil, nil),
					box.AppendStco(nil, []uint32{uint32(ps)}),
				)
			},
		},
		{
			// A single run spanning four chunks with a huge samples_per_chunk. The
			// product runChunks*samples_per_chunk must be computed overflow-safely
			// and rejected before the sample_count comparison.
			name:    "runchunks times spc overflow shape",
			n:       1,
			payload: []byte{0, 1, 2, 3, 4},
			build: func(ps int64) []byte {
				return joinStbl(
					stsd,
					stscBox(box.StscEntry{FirstChunk: 1, SamplesPerChunk: 0xFFFFFFFF, SampleDescriptionIndex: 1}),
					box.AppendStsz(nil, []uint32{5}),
					box.AppendStco(nil, []uint32{uint32(ps), uint32(ps), uint32(ps), uint32(ps)}),
				)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := assembleMoovStbl(48000, 1, tc.n, tc.payload, tc.build)
			err := newReaderNoPanic(t, data)
			if !errors.Is(err, ErrCorrupt) {
				t.Fatalf("NewReader error = %v, want wrapped ErrCorrupt", err)
			}
		})
	}
}

// newReaderNoPanic runs NewReader alone, recovering any panic into a fatal test
// failure, and returns its error. Unlike noPanicNewReader it does not drain
// ReadFrame, so it pins failures that must surface at construction time (the
// expandStsc geometry build happens inside NewReader).
func newReaderNoPanic(t *testing.T, data []byte) (err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panic in NewReader on malformed input: %v", p)
		}
	}()
	_, err = NewReader(bytes.NewReader(data))
	return err
}

// TestExpandStscValidMultiChunk is the positive control for the expandStsc fix:
// a genuinely valid two-chunk table (chunk 1 holds two samples, chunk 2 holds
// three) must still parse and read back byte for byte, proving the hardened
// bounds checks did not start rejecting well-formed multi-chunk files. The Writer
// only ever emits a single chunk, so this table is assembled by hand.
func TestExpandStscValidMultiChunk(t *testing.T) {
	const n = 5
	frames := synthFrames(n)
	payload, sizes := framePayload(frames)

	// chunk 1: samples 0..1; chunk 2: samples 2..4.
	chunk1Len := int64(len(frames[0]) + len(frames[1]))
	stsd := box.AppendStsd(nil, 1, 48000, ascMono48k)

	data := assembleMoovStbl(48000, 1, n, payload, func(ps int64) []byte {
		return joinStbl(
			stsd,
			box.AppendStts(nil, n, samplesPerFrame),
			stscBox(
				box.StscEntry{FirstChunk: 1, SamplesPerChunk: 2, SampleDescriptionIndex: 1},
				box.StscEntry{FirstChunk: 2, SamplesPerChunk: 3, SampleDescriptionIndex: 1},
			),
			box.AppendStsz(nil, sizes),
			box.AppendStco(nil, []uint32{uint32(ps), uint32(ps + chunk1Len)}),
		)
	})

	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader on valid two-chunk file: %v", err)
	}
	if fc := r.Info().FrameCount; fc != n {
		t.Errorf("FrameCount = %d, want %d", fc, n)
	}
	got := collectFrames(t, r)
	if len(got) != n {
		t.Fatalf("read %d frames, want %d", len(got), n)
	}
	for i := range frames {
		if !bytes.Equal(got[i], frames[i]) {
			t.Errorf("frame %d = % x, want % x", i, got[i], frames[i])
		}
	}
}

// TestMalformedMoovNegatives closes the demuxer coverage gaps the gate flagged.
// Each case hand-assembles a moov that is structurally parseable but violates one
// container invariant, and asserts NewReader (draining ReadFrame) rejects it with
// the right sentinel and never panics.
func TestMalformedMoovNegatives(t *testing.T) {
	stsd := box.AppendStsd(nil, 1, 48000, ascMono48k)

	tests := []struct {
		name    string
		n       int
		payload []byte
		build   func(payloadStart int64) []byte
		want    error
	}{
		{
			name:    "missing stsc",
			n:       3,
			payload: make([]byte, 11), // 11 bytes, enough for the three sizes below
			build: func(ps int64) []byte {
				return joinStbl(
					stsd,
					box.AppendStts(nil, 3, samplesPerFrame),
					box.AppendStsz(nil, []uint32{3, 4, 4}),
					box.AppendStco(nil, []uint32{uint32(ps)}),
				)
			},
			want: ErrCorrupt,
		},
		{
			name:    "missing stsz",
			n:       3,
			payload: make([]byte, 11),
			build: func(ps int64) []byte {
				return joinStbl(
					stsd,
					box.AppendStts(nil, 3, samplesPerFrame),
					box.AppendStsc(nil, 1, 3, 1),
					box.AppendStco(nil, []uint32{uint32(ps)}),
				)
			},
			want: ErrCorrupt,
		},
		{
			name:    "co64 offset past eof",
			n:       1,
			payload: []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, // 10 bytes for the one sample
			build: func(_ int64) []byte {
				return joinStbl(
					stsd,
					box.AppendStts(nil, 1, samplesPerFrame),
					box.AppendStsc(nil, 1, 1, 1),
					box.AppendStsz(nil, []uint32{10}),
					box.AppendCo64(nil, []uint64{1 << 40}), // far past EOF, still < 1<<62
				)
			},
			want: ErrCorrupt,
		},
		{
			name:    "zero length frame",
			n:       1,
			payload: nil, // one sample of size 0
			build: func(ps int64) []byte {
				return joinStbl(
					stsd,
					box.AppendStts(nil, 1, samplesPerFrame),
					box.AppendStsc(nil, 1, 1, 1),
					box.AppendStsz(nil, []uint32{0}),
					box.AppendStco(nil, []uint32{uint32(ps)}),
				)
			},
			want: ErrCorrupt,
		},
		{
			name:    "stts disagrees with stsz",
			n:       3,
			payload: make([]byte, 11),
			build: func(ps int64) []byte {
				return joinStbl(
					stsd,
					box.AppendStts(nil, 5, samplesPerFrame), // claims 5, stsz has 3
					box.AppendStsc(nil, 1, 3, 1),
					box.AppendStsz(nil, []uint32{3, 4, 4}),
					box.AppendStco(nil, []uint32{uint32(ps)}),
				)
			},
			want: ErrCorrupt,
		},
		{
			name:    "esds object type not aac",
			n:       3,
			payload: make([]byte, 11),
			build: func(ps int64) []byte {
				return joinStbl(
					stsdObjType(t, 1, 48000, ascMono48k, 0x67), // MPEG-2 AAC main, not 0x40
					box.AppendStts(nil, 3, samplesPerFrame),
					box.AppendStsc(nil, 1, 3, 1),
					box.AppendStsz(nil, []uint32{3, 4, 4}),
					box.AppendStco(nil, []uint32{uint32(ps)}),
				)
			},
			want: ErrUnsupported,
		},
		{
			name:    "sample entry not mp4a",
			n:       3,
			payload: make([]byte, 11),
			build: func(ps int64) []byte {
				return joinStbl(
					stsdCodec("ac-3"),
					box.AppendStts(nil, 3, samplesPerFrame),
					box.AppendStsc(nil, 1, 3, 1),
					box.AppendStsz(nil, []uint32{3, 4, 4}),
					box.AppendStco(nil, []uint32{uint32(ps)}),
				)
			},
			want: ErrUnsupported,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := assembleMoovStbl(48000, 1, tc.n, tc.payload, tc.build)
			err := noPanicNewReader(t, data)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}
