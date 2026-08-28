// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tphakala/go-m4a/internal/box"
)

// fragAU is one access unit and the sample duration to record for it in a
// fragmented round-trip fixture.
type fragAU struct {
	au  []byte
	dur uint32
}

// buildFragmentedStream produces a complete fragmented (CMAF) byte stream: the
// init segment for cfg followed by one media segment per element of segments. It
// is the natural fixture for the demuxer: the writer under test produces exactly
// the moof/trun layout the reader must reverse.
func buildFragmentedStream(tb testing.TB, cfg WriterConfig, segments [][]fragAU) []byte {
	tb.Helper()
	init, err := InitSegment(cfg)
	if err != nil {
		tb.Fatalf("InitSegment: %v", err)
	}
	fw, err := NewFragmentWriter(cfg)
	if err != nil {
		tb.Fatalf("NewFragmentWriter: %v", err)
	}
	out := append([]byte(nil), init...)
	for i, seg := range segments {
		for _, s := range seg {
			if err := fw.WriteFrameDuration(s.au, s.dur); err != nil {
				tb.Fatalf("segment %d WriteFrameDuration: %v", i, err)
			}
		}
		out, err = fw.AppendSegment(out)
		if err != nil {
			tb.Fatalf("segment %d AppendSegment: %v", i, err)
		}
	}
	return out
}

// drainFrames reads every access unit from rd, failing on any error other than
// the terminating io.EOF.
func drainFrames(t *testing.T, rd *Reader) [][]byte {
	t.Helper()
	var got [][]byte
	for {
		au, err := rd.ReadFrame()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		got = append(got, au)
	}
}

// assertFramesEqual checks that the reader returned exactly the access units that
// were written, in order and byte for byte.
func assertFramesEqual(t *testing.T, got, want [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("read %d frames, wrote %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("frame %d: read %d bytes, wrote %d; bytes differ", i, len(got[i]), len(want[i]))
		}
	}
}

func uniformSegment(frames [][]byte, dur uint32) []fragAU {
	seg := make([]fragAU, len(frames))
	for i, f := range frames {
		seg[i] = fragAU{au: f, dur: dur}
	}
	return seg
}

// TestFragmentRoundTripAAC exercises the uniform-duration path: the writer emits a
// tfhd default_sample_duration and a trun carrying only sizes, so the demuxer must
// resolve every sample's duration from the tfhd default.
func TestFragmentRoundTripAAC(t *testing.T) {
	t.Parallel()
	frames := synthFrames(10)
	data := buildFragmentedStream(t, aacFragmentConfig(), [][]fragAU{uniformSegment(frames, 1024)})

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if rd.Info().Codec != CodecAACLC {
		t.Errorf("Codec = %v, want AAC-LC", rd.Info().Codec)
	}
	if rd.Info().SampleRate != 48000 || rd.Info().Channels != 1 {
		t.Errorf("format = %d Hz / %d ch, want 48000/1", rd.Info().SampleRate, rd.Info().Channels)
	}
	if rd.Info().FrameCount != len(frames) {
		t.Errorf("FrameCount = %d, want %d", rd.Info().FrameCount, len(frames))
	}
	assertFramesEqual(t, drainFrames(t, rd), frames)
}

// TestFragmentRoundTripOpus exercises the per-sample-duration path with Opus.
func TestFragmentRoundTripOpus(t *testing.T) {
	t.Parallel()
	cfg := WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 1, OpusPreSkip: 312}
	frames := synthFrames(7)
	seg := make([]fragAU, len(frames))
	for i, f := range frames {
		// Alternate durations so the writer emits per-sample trun durations rather
		// than a tfhd default.
		dur := uint32(960)
		if i%2 == 1 {
			dur = 480
		}
		seg[i] = fragAU{au: f, dur: dur}
	}
	data := buildFragmentedStream(t, cfg, [][]fragAU{seg})

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if rd.Info().Codec != CodecOpus {
		t.Errorf("Codec = %v, want Opus", rd.Info().Codec)
	}
	if rd.Info().SampleRate != 48000 || rd.Info().Channels != 1 {
		t.Errorf("format = %d Hz / %d ch, want 48000/1", rd.Info().SampleRate, rd.Info().Channels)
	}
	if rd.Info().EncoderDelay != 312 {
		t.Errorf("EncoderDelay = %d, want 312 (dOps PreSkip via elst)", rd.Info().EncoderDelay)
	}
	assertFramesEqual(t, drainFrames(t, rd), frames)
}

// TestFragmentRoundTripFLAC exercises the per-sample-duration path with FLAC and a
// non-48 kHz rate resolved from the dfLa STREAMINFO.
func TestFragmentRoundTripFLAC(t *testing.T) {
	t.Parallel()
	cfg := WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: flacStreamInfo(44100, 1), EncoderDelay: NoEdit}
	frames := synthFrames(6)
	seg := make([]fragAU, len(frames))
	for i, f := range frames {
		dur := uint32(4096)
		if i == 2 {
			dur = 2048 // one short final-ish block, forcing per-sample durations
		}
		seg[i] = fragAU{au: f, dur: dur}
	}
	data := buildFragmentedStream(t, cfg, [][]fragAU{seg})

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if rd.Info().Codec != CodecFLAC {
		t.Errorf("Codec = %v, want FLAC", rd.Info().Codec)
	}
	if rd.Info().SampleRate != 44100 || rd.Info().Channels != 1 {
		t.Errorf("format = %d Hz / %d ch, want 44100/1", rd.Info().SampleRate, rd.Info().Channels)
	}
	assertFramesEqual(t, drainFrames(t, rd), frames)
}

// TestFragmentMultiSegment checks that access units stream in order across several
// moof/mdat boundaries, which is where the shared cursor's chunk-to-chunk jump is
// exercised.
func TestFragmentMultiSegment(t *testing.T) {
	t.Parallel()
	all := synthFrames(15)
	segments := [][]fragAU{
		uniformSegment(all[0:4], 1024),
		uniformSegment(all[4:10], 1024),
		uniformSegment(all[10:15], 1024),
	}
	data := buildFragmentedStream(t, aacFragmentConfig(), segments)

	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if rd.Info().FrameCount != len(all) {
		t.Errorf("FrameCount = %d, want %d", rd.Info().FrameCount, len(all))
	}
	assertFramesEqual(t, drainFrames(t, rd), all)
}

// TestFragmentDuration checks that Info.Duration is the sum of the fragments'
// sample durations at the media timescale, since the init segment's own durations
// are all zero.
func TestFragmentDuration(t *testing.T) {
	t.Parallel()
	frames := synthFrames(48) // 48 * 1024 / 48000 = 1.024 s
	data := buildFragmentedStream(t, aacFragmentConfig(), [][]fragAU{uniformSegment(frames, 1024)})
	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	want := time.Duration(float64(48*1024) / 48000 * float64(time.Second))
	got := rd.Info().Duration
	if got < want-want/100 || got > want+want/100 {
		t.Errorf("Duration = %v, want about %v", got, want)
	}
}

// TestFragmentRawStream checks the length-prefixed RawStream framing works over a
// fragmented source, mirroring the plain-path guarantee.
func TestFragmentRawStream(t *testing.T) {
	t.Parallel()
	frames := synthFrames(5)
	data := buildFragmentedStream(t, aacFragmentConfig(), [][]fragAU{uniformSegment(frames, 1024)})
	rd, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	raw, err := io.ReadAll(rd.RawStream())
	if err != nil {
		t.Fatalf("RawStream: %v", err)
	}
	// Each frame is a 2-byte big-endian length prefix followed by its bytes; verify
	// every prefix and payload, not just the total length, so a misframed or
	// misordered stream cannot pass on byte count alone.
	off := 0
	for i, f := range frames {
		if off+2 > len(raw) {
			t.Fatalf("frame %d length prefix past end of stream", i)
		}
		n := int(binary.BigEndian.Uint16(raw[off:]))
		off += 2
		if n != len(f) {
			t.Errorf("frame %d prefix = %d, want %d", i, n, len(f))
		}
		if off+n > len(raw) || !bytes.Equal(raw[off:off+n], f) {
			t.Errorf("frame %d payload differs from the written access unit", i)
		}
		off += n
	}
	if off != len(raw) {
		t.Errorf("RawStream has %d trailing bytes after the last frame", len(raw)-off)
	}
}

// handContainer wraps children in a plain box header of the given four-character
// type, for tests that hand-build a movie fragment.
func handContainer(typ string, children []byte) []byte {
	out := make([]byte, 0, 8+len(children))
	out = binary.BigEndian.AppendUint32(out, uint32(8+len(children)))
	out = append(out, typ...)
	return append(out, children...)
}

// handTfhd builds a tfhd carrying an explicit base_data_offset (absolute file
// position), a form AppendTfhd never emits (it always uses default-base-is-moof).
// This is the shape a foreign muxer or a relocated segment produces.
func handTfhd(trackID uint32, baseDataOffset uint64) []byte {
	body := binary.BigEndian.AppendUint32(nil, trackID)
	body = binary.BigEndian.AppendUint64(body, baseDataOffset)
	const flags = 0x000001 // base-data-offset-present
	out := binary.BigEndian.AppendUint32(nil, uint32(8+4+len(body)))
	out = append(out, "tfhd"...)
	out = append(out, 0, byte(flags>>16), byte(flags>>8), byte(flags))
	return append(out, body...)
}

// TestFragmentForeignTrackAndBaseOffset hand-builds a media segment whose moof
// carries a foreign (video) track fragment before the audio one, each addressed
// by an absolute base_data_offset. It proves the demuxer binds fragments by
// track_ID (skipping the video traf) and resolves base_data_offset, the two paths
// a real CMAF restream from another muxer exercises but the writer never emits.
func TestFragmentForeignTrackAndBaseOffset(t *testing.T) {
	t.Parallel()
	stream, audio := fragmentedStreamWithExtraTraf(t, func(base uint64) []byte {
		return handContainer("traf", concatBytes(
			handTfhd(2, base), // a video track: bound by track_ID, never by order
			box.AppendTfdt(nil, 0),
			box.AppendTrun(nil, 0, []uint32{extraTrafPayloadLen}, nil),
		))
	})
	rd, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if rd.Info().FrameCount != len(audio) {
		t.Errorf("FrameCount = %d, want %d (video traf must be skipped)", rd.Info().FrameCount, len(audio))
	}
	assertFramesEqual(t, drainFrames(t, rd), audio)
}

// fragBox builds a FullBox (8-byte header + version/flags + body) for hand-built
// movie-fragment boxes the writer never emits.
func fragBox(typ string, version uint8, flags uint32, body []byte) []byte {
	out := make([]byte, 0, 12+len(body))
	out = binary.BigEndian.AppendUint32(out, uint32(12+len(body)))
	out = append(out, typ...)
	out = append(out, version, byte(flags>>16), byte(flags>>8), byte(flags))
	return append(out, body...)
}

// handTfhdDefaultSize builds a tfhd with default-base-is-moof and a
// default_sample_size, so a size-less trun resolves each sample's size from it.
func handTfhdDefaultSize(trackID, defaultSize uint32) []byte {
	const flags = 0x020000 | 0x000010 // default-base-is-moof | default-sample-size-present
	body := binary.BigEndian.AppendUint32(nil, trackID)
	body = binary.BigEndian.AppendUint32(body, defaultSize)
	return fragBox("tfhd", 0, flags, body)
}

// handTrunDataOffsetOnly builds a well-formed trun carrying only data_offset and
// sample_count, with no per-sample records, so every sample's size comes from the
// tfhd/trex default.
func handTrunDataOffsetOnly(sampleCount uint32, dataOffset int32) []byte {
	return handTrunVersion(0, sampleCount, dataOffset)
}

// TestFragmentTfhdDefaultSampleSize exercises the resolveSampleSize fallback: the
// trun carries no per-sample sizes, so each sample's size comes from the tfhd
// default_sample_size. The writer never emits this shape (it always writes
// per-sample sizes), so it is hand-built. It runs over both child orders, which
// also pins the walk's independence from where the tfhd sits.
func TestFragmentTfhdDefaultSampleSize(t *testing.T) {
	t.Parallel()
	const n, sz = 4, 20
	// The two child orders assert different things from one fixture. tfhd first is
	// the shape every muxer writes, and exercises the resolveSampleSize fallback.
	// tfhd last exercises nothing about sizes and everything about the two-pass
	// walk: ISO puts the header first, but the track_ID check now runs before the
	// runs are read, so the header must still be found wherever it sits or a run
	// would be skipped for want of a header that was merely written later.
	for _, tc := range []struct {
		name      string
		tfhdFirst bool
	}{
		{"tfhd first", true},
		{"tfhd after trun", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			init, err := InitSegment(aacFragmentConfig()) // audio track_ID 1
			if err != nil {
				t.Fatalf("InitSegment: %v", err)
			}
			payload := make([]byte, n*sz)
			for i := range payload {
				payload[i] = byte(i)
			}
			buildMoof := func(dataOffset int32) []byte {
				tfhd, trun := handTfhdDefaultSize(1, sz), handTrunDataOffsetOnly(n, dataOffset)
				children := concatBytes(tfhd, trun)
				if !tc.tfhdFirst {
					children = concatBytes(trun, tfhd)
				}
				return handContainer("moof", concatBytes(box.AppendMfhd(nil, 1), handContainer("traf", children)))
			}
			moofLen := int64(len(buildMoof(0)))
			moof := buildMoof(int32(moofLen + box.MdatShortHeaderSize))
			stream := concatBytes(init, moof, box.AppendMdat(nil, payload))

			rd, err := NewReader(bytes.NewReader(stream))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			frames := drainFrames(t, rd)
			if len(frames) != n {
				t.Fatalf("got %d frames, want %d", len(frames), n)
			}
			for i, f := range frames {
				if !bytes.Equal(f, payload[i*sz:(i+1)*sz]) {
					t.Errorf("frame %d (size %d) differs from the default-sized slice", i, len(f))
				}
			}
		})
	}
}

// TestFragmentZeroSizeRejected checks that a trun sample resolving to size 0 is
// rejected as ErrCorrupt rather than yielding a zero-length access unit.
func TestFragmentZeroSizeRejected(t *testing.T) {
	t.Parallel()
	init, err := InitSegment(aacFragmentConfig())
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	sizes := []uint32{10, 0, 10} // the middle sample declares zero size
	buildMoof := func(dataOffset int32) []byte {
		traf := handContainer("traf", concatBytes(
			box.AppendTfhd(nil, 1, 0), // default-base-is-moof
			box.AppendTrun(nil, dataOffset, sizes, nil),
		))
		return handContainer("moof", concatBytes(box.AppendMfhd(nil, 1), traf))
	}
	moofLen := int64(len(buildMoof(0)))
	moof := buildMoof(int32(moofLen + box.MdatShortHeaderSize))
	stream := concatBytes(init, moof, box.AppendMdat(nil, make([]byte, 20)))

	if _, err := NewReader(bytes.NewReader(stream)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("NewReader = %v, want ErrCorrupt", err)
	}
}

// TestFragmentTrafWithoutBase checks that a track fragment whose tfhd sets neither
// base_data_offset nor default-base-is-moof is rejected as ErrUnsupported: its
// base would be the end of a preceding track's data, which this single-track
// demuxer does not track.
func TestFragmentTrafWithoutBase(t *testing.T) {
	t.Parallel()
	init, err := InitSegment(aacFragmentConfig())
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	tfhd := fragBox("tfhd", 0, 0, binary.BigEndian.AppendUint32(nil, 1)) // flags 0: no base
	traf := handContainer("traf", concatBytes(tfhd, box.AppendTrun(nil, 100, []uint32{10}, nil)))
	moof := handContainer("moof", concatBytes(box.AppendMfhd(nil, 1), traf))
	stream := concatBytes(init, moof, box.AppendMdat(nil, make([]byte, 10)))

	if _, err := NewReader(bytes.NewReader(stream)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("NewReader = %v, want ErrUnsupported", err)
	}
}

// handTrunNoOffset builds a trun with sample-size-present but NOT
// data-offset-present, the shape whose data continues immediately after the
// previous run in the same traf (ISO/IEC 14496-12 8.8.8). AppendTrun always sets
// data-offset-present, so this case needs a hand-built box.
func handTrunNoOffset(sizes []uint32) []byte {
	flags := uint32(0x000200) // sample-size-present only
	body := binary.BigEndian.AppendUint32(nil, uint32(len(sizes)))
	for _, s := range sizes {
		body = binary.BigEndian.AppendUint32(body, s)
	}
	out := binary.BigEndian.AppendUint32(nil, uint32(8+4+len(body)))
	out = append(out, "trun"...)
	out = append(out, 0, byte(flags>>16), byte(flags>>8), byte(flags))
	return append(out, body...)
}

// TestFragmentMultiTrunContiguous puts two runs in one traf where the second omits
// data-offset-present, so its samples continue right after the first run's data.
// This exercises the running dataCursor, a path the writer never emits (it always
// sets data-offset-present) but a foreign muxer does.
func TestFragmentMultiTrunContiguous(t *testing.T) {
	t.Parallel()
	init, err := InitSegment(aacFragmentConfig()) // audio track_ID is 1, default-base-is-moof
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	all := synthFrames(5)
	var payload []byte
	sizes := make([]uint32, len(all))
	for i, f := range all {
		payload = append(payload, f...)
		sizes[i] = uint32(len(f))
	}
	sizes1, sizes2 := sizes[:3], sizes[3:]

	// default-base-is-moof: the base is the moof file offset, so the first run's
	// data_offset is the moof length plus the mdat header. Measure the moof, then
	// rebuild with the real offset (its length is offset-value-independent).
	// AppendTfhd with duration 0 emits exactly the default-base-is-moof header.
	buildMoof := func(dataOffset int32) []byte {
		traf := handContainer("traf", concatBytes(
			box.AppendTfhd(nil, 1, 0),
			box.AppendTrun(nil, dataOffset, sizes1, nil),
			handTrunNoOffset(sizes2),
		))
		return handContainer("moof", concatBytes(box.AppendMfhd(nil, 1), traf))
	}
	moofLen := int64(len(buildMoof(0)))
	dataOffset := int32(moofLen + box.MdatShortHeaderSize)
	moof := buildMoof(dataOffset)
	stream := concatBytes(init, moof, box.AppendMdat(nil, payload))

	rd, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if rd.Info().FrameCount != len(all) {
		t.Errorf("FrameCount = %d, want %d", rd.Info().FrameCount, len(all))
	}
	assertFramesEqual(t, drainFrames(t, rd), all)
}

func concatBytes(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// TestFragmentNoMatchingTrack checks that a fragmented stream whose fragments all
// belong to a different track than the selected audio track is rejected as
// ErrCorrupt, rather than opening as a reader that silently reports zero frames.
func TestFragmentNoMatchingTrack(t *testing.T) {
	t.Parallel()
	init, err := InitSegment(aacFragmentConfig()) // audio track_ID 1
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	sizes := []uint32{10}
	buildMoof := func(dataOffset int32) []byte {
		traf := handContainer("traf", concatBytes(
			box.AppendTfhd(nil, 2, 0), // track_ID 2, not the selected audio track 1
			box.AppendTrun(nil, dataOffset, sizes, nil),
		))
		return handContainer("moof", concatBytes(box.AppendMfhd(nil, 1), traf))
	}
	moofLen := int64(len(buildMoof(0)))
	moof := buildMoof(int32(moofLen + box.MdatShortHeaderSize))
	stream := concatBytes(init, moof, box.AppendMdat(nil, make([]byte, 10)))

	if _, err := NewReader(bytes.NewReader(stream)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("NewReader = %v, want ErrCorrupt", err)
	}
}

// TestFragmentTruncated checks a stream cut mid-segment is rejected as ErrCorrupt
// (or ErrUnsupported), never a panic.
func TestFragmentTruncated(t *testing.T) {
	t.Parallel()
	frames := synthFrames(8)
	data := buildFragmentedStream(t, aacFragmentConfig(), [][]fragAU{uniformSegment(frames, 1024)})
	for _, cut := range []int{1, 5, 20, len(data) / 2, len(data) - 3} {
		if cut <= 0 || cut >= len(data) {
			continue
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("cut %d panicked: %v", cut, r)
				}
			}()
			rd, err := NewReader(bytes.NewReader(data[:cut]))
			if err != nil {
				if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrUnsupported) {
					t.Errorf("cut %d: err = %v, want ErrCorrupt/ErrUnsupported", cut, err)
				}
				return
			}
			// If it opened, draining must still not panic and any error is clean.
			for {
				_, rerr := rd.ReadFrame()
				if rerr == nil {
					continue
				}
				if !errors.Is(rerr, io.EOF) && !errors.Is(rerr, ErrCorrupt) {
					t.Errorf("cut %d: ReadFrame err = %v, want EOF/ErrCorrupt", cut, rerr)
				}
				return
			}
		}()
	}
}

// handTrunVersion builds a trun with a chosen version byte and no per-sample
// records, so a test can emit the reserved version ParseTrun rejects.
func handTrunVersion(version uint8, sampleCount uint32, dataOffset int32) []byte {
	body := binary.BigEndian.AppendUint32(nil, sampleCount)
	body = binary.BigEndian.AppendUint32(body, uint32(dataOffset))
	return fragBox("trun", version, 0x000001, body) // data-offset-present
}

// handTfdtVersion builds a tfdt with a chosen version byte, so a test can emit
// the reserved version ParseTfdt rejects.
func handTfdtVersion(version uint8) []byte {
	return fragBox("tfdt", version, 0, binary.BigEndian.AppendUint32(nil, 0))
}

// handTfhdBaseAndSize builds a tfhd carrying an absolute base_data_offset and a
// default_sample_size, in that ISO field order. A record-free trun under it
// resolves every sample's size from the header, which lets a test hold the whole
// fragment well-formed and vary exactly one child.
func handTfhdBaseAndSize(trackID uint32, baseDataOffset uint64, defaultSize uint32) []byte {
	const flags = 0x000001 | 0x000010 // base-data-offset-present | default-sample-size-present
	body := binary.BigEndian.AppendUint32(nil, trackID)
	body = binary.BigEndian.AppendUint64(body, baseDataOffset)
	body = binary.BigEndian.AppendUint32(body, defaultSize)
	return fragBox("tfhd", 0, flags, body)
}

// handTfhdDurationIsEmpty builds a tfhd with default-base-is-moof and the
// duration-is-empty flag, which declares a fragment carrying no samples.
func handTfhdDurationIsEmpty(trackID uint32) []byte {
	const flags = 0x020000 | 0x010000 // default-base-is-moof | duration-is-empty
	return fragBox("tfhd", 0, flags, binary.BigEndian.AppendUint32(nil, trackID))
}

// extraTrafPayload is the mdat content the extra traf addresses, placed ahead of
// the audio. Its length is exported as a constant so a traf can declare it as a
// default_sample_size and consume it in one well-formed sample.
const (
	extraTrafPayload    = "VIDEODATA-not-audio-and-must-be-skipped"
	extraTrafPayloadLen = uint32(len(extraTrafPayload))
)

// fragmentedStreamWithExtraTraf hand-builds a fragmented stream whose single moof
// carries extraTraf (given the absolute file offset of its data) ahead of a
// well-formed audio traf for track_ID 1, and whose mdat carries that traf's data
// followed by the audio. It returns the stream and the access units a reader must
// demux from it. extraTraf must return the same number of bytes whatever base it
// is given, since the moof is built twice to measure its length.
func fragmentedStreamWithExtraTraf(tb testing.TB, extraTraf func(base uint64) []byte) (stream []byte, audio [][]byte) {
	tb.Helper()
	init, err := InitSegment(aacFragmentConfig()) // audio track_ID is 1
	if err != nil {
		tb.Fatalf("InitSegment: %v", err)
	}
	audio = synthFrames(3)
	var audioBytes []byte
	audioSizes := make([]uint32, len(audio))
	audioDurs := make([]uint32, len(audio))
	for i, f := range audio {
		audioBytes = append(audioBytes, f...)
		audioSizes[i] = uint32(len(f))
		audioDurs[i] = 1024
	}
	extraBytes := []byte(extraTrafPayload)

	buildMoof := func(extraBase, audioBase uint64) []byte {
		audioTraf := handContainer("traf", concatBytes(
			handTfhd(1, audioBase),
			box.AppendTfdt(nil, 0),
			box.AppendTrun(nil, 0, audioSizes, audioDurs),
		))
		return handContainer("moof", concatBytes(box.AppendMfhd(nil, 1), extraTraf(extraBase), audioTraf))
	}
	// base_data_offset is an absolute file position pointing past the moof into the
	// mdat, so build the moof once to measure it, then rebuild with real offsets.
	moofLen := int64(len(buildMoof(0, 0)))
	mdatPayloadAbs := uint64(int64(len(init)) + moofLen + box.MdatShortHeaderSize)
	// Enforce the length-invariance the doc requires rather than trusting callers:
	// a traf that changed size with its base would shift every absolute offset in
	// the fixture, and the test built on it would pass or fail for a reason that
	// has nothing to do with what it asserts.
	if a, b := len(extraTraf(0)), len(extraTraf(mdatPayloadAbs)); a != b {
		tb.Fatalf("extraTraf length varies with base: %d bytes at 0, %d at %d", a, b, mdatPayloadAbs)
	}
	moof := buildMoof(mdatPayloadAbs, mdatPayloadAbs+uint64(len(extraBytes)))
	stream = concatBytes(init, moof, box.AppendMdat(nil, concatBytes(extraBytes, audioBytes)))
	return stream, audio
}

// TestFragmentForeignTrafMalformedTrunIgnored pins the strictness contract the
// track_ID check buys by running before the runs are parsed: a foreign track's
// trun is never decoded, so a malformed one no longer fails the open. The audio
// track must still demux exactly.
func TestFragmentForeignTrafMalformedTrunIgnored(t *testing.T) {
	t.Parallel()
	stream, audio := fragmentedStreamWithExtraTraf(t, func(base uint64) []byte {
		return handContainer("traf", concatBytes(
			handTfhd(2, base),
			box.AppendTfdt(nil, 0),
			handTrunVersion(2, 1, 0), // reserved version: ParseTrun rejects it
		))
	})
	rd, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewReader: %v, want success (a foreign track's trun is not this demuxer's to validate)", err)
	}
	assertFramesEqual(t, drainFrames(t, rd), audio)
}

// TestFragmentForeignTrafMalformedTfdtIgnored is the same contract for the other
// box the selected track's walk parses for validation.
func TestFragmentForeignTrafMalformedTfdtIgnored(t *testing.T) {
	t.Parallel()
	stream, audio := fragmentedStreamWithExtraTraf(t, func(base uint64) []byte {
		return handContainer("traf", concatBytes(
			handTfhd(2, base),
			handTfdtVersion(2), // reserved version: ParseTfdt rejects it
			box.AppendTrun(nil, 0, []uint32{1}, nil),
		))
	})
	rd, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewReader: %v, want success (a foreign track's tfdt is not this demuxer's to validate)", err)
	}
	assertFramesEqual(t, drainFrames(t, rd), audio)
}

// selectedTrafStream builds a stream whose extra traf belongs to the SELECTED
// track and carries children, so a test can vary one child and hold the rest
// well-formed. The tfhd declares a default sample size covering the whole extra
// payload, which matters: without a size source the run fails with "zero or
// unresolved size" regardless of the child under test, and a test built on it
// would pass even with the parser's validation removed.
func selectedTrafStream(tb testing.TB, children func(base uint64) []byte) []byte {
	tb.Helper()
	stream, _ := fragmentedStreamWithExtraTraf(tb, func(base uint64) []byte {
		return handContainer("traf", concatBytes(
			handTfhdBaseAndSize(1, base, extraTrafPayloadLen),
			children(base),
		))
	})
	return stream
}

// TestFragmentSelectedTrafMalformedTrunRejected is the other half of the
// contract: the relaxation is scoped to foreign tracks, and a malformed trun in
// the selected track's own traf still fails the open. Without this the track_ID
// check could be moved far enough to skip validating the audio runs too.
//
// The well-formed case is asserted first. It is what makes the failing case
// evidence: it proves the fixture opens cleanly when only the trun's version
// changes, so the rejection below can only be the version.
func TestFragmentSelectedTrafMalformedTrunRejected(t *testing.T) {
	t.Parallel()
	build := func(trun []byte) []byte {
		return selectedTrafStream(t, func(uint64) []byte {
			return concatBytes(box.AppendTfdt(nil, 0), trun)
		})
	}

	if _, err := NewReader(bytes.NewReader(build(handTrunVersion(0, 1, 0)))); err != nil {
		t.Fatalf("NewReader with a well-formed run: %v, want success", err)
	}

	_, err := NewReader(bytes.NewReader(build(handTrunVersion(2, 1, 0))))
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("NewReader = %v, want ErrCorrupt", err)
	}
	if !strings.Contains(err.Error(), "trun version 2") {
		t.Errorf("error = %q, want it to name the trun version as the cause", err)
	}
}

// TestFragmentSelectedTrafMalformedTfdtRejected mirrors the trun case for the
// other box the selected track's walk parses. The foreign half of this pair is
// TestFragmentForeignTrafMalformedTfdtIgnored, and without this test the tfdt
// check could be deleted outright with the whole suite still green.
func TestFragmentSelectedTrafMalformedTfdtRejected(t *testing.T) {
	t.Parallel()
	build := func(tfdt []byte) []byte {
		return selectedTrafStream(t, func(uint64) []byte {
			return concatBytes(tfdt, handTrunVersion(0, 1, 0))
		})
	}

	if _, err := NewReader(bytes.NewReader(build(box.AppendTfdt(nil, 0)))); err != nil {
		t.Fatalf("NewReader with a well-formed tfdt: %v, want success", err)
	}

	_, err := NewReader(bytes.NewReader(build(handTfdtVersion(2))))
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("NewReader = %v, want ErrCorrupt", err)
	}
	if !strings.Contains(err.Error(), "tfdt version 2") {
		t.Errorf("error = %q, want it to name the tfdt version as the cause", err)
	}
}

// TestFragmentMissingBasePreemptsMalformedChild pins the error-precedence change
// the two-pass split introduced. Resolving the base moved ahead of parsing the
// runs, so a selected traf that is BOTH missing a base offset AND carrying a
// malformed child now reports the missing base rather than the malformed box.
// Both were failures before and are failures now, but the sentinel a caller
// branches on flipped from ErrCorrupt to ErrUnsupported, so it is pinned here
// rather than left to be discovered.
func TestFragmentMissingBasePreemptsMalformedChild(t *testing.T) {
	t.Parallel()
	// A tfhd with no flags at all: no base_data_offset, no default-base-is-moof.
	noBaseTfhd := fragBox("tfhd", 0, 0, binary.BigEndian.AppendUint32(nil, 1))
	for _, tc := range []struct {
		name  string
		child []byte
	}{
		{"malformed trun", handTrunVersion(2, 1, 0)},
		{"malformed tfdt", handTfdtVersion(2)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stream, _ := fragmentedStreamWithExtraTraf(t, func(uint64) []byte {
				return handContainer("traf", concatBytes(noBaseTfhd, tc.child))
			})
			_, err := NewReader(bytes.NewReader(stream))
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("NewReader = %v, want ErrUnsupported (the missing base preempts the malformed child)", err)
			}
			if errors.Is(err, ErrCorrupt) {
				t.Errorf("error = %v, want ErrUnsupported only", err)
			}
		})
	}
}

// TestFragmentForeignTrafMalformedTfhdRejected pins the one box a foreign traf
// must still get right: track binding reads the tfhd, so an unparsable one leaves
// the demuxer unable to tell whose fragment it is rather than free to skip it.
func TestFragmentForeignTrafMalformedTfhdRejected(t *testing.T) {
	t.Parallel()
	stream, _ := fragmentedStreamWithExtraTraf(t, func(_ uint64) []byte {
		// A tfhd body too short to hold even track_ID.
		return handContainer("traf", fragBox("tfhd", 0, 0, []byte{0, 0, 0}))
	})
	_, err := NewReader(bytes.NewReader(stream))
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("NewReader = %v, want ErrCorrupt", err)
	}
}

// TestFragmentDurationIsEmptyTrafContributesNothing checks that a selected-track
// traf flagged duration-is-empty adds no samples even though it carries a
// well-formed run, which is what the flag declares.
func TestFragmentDurationIsEmptyTrafContributesNothing(t *testing.T) {
	t.Parallel()
	stream, audio := fragmentedStreamWithExtraTraf(t, func(_ uint64) []byte {
		return handContainer("traf", concatBytes(
			handTfhdDurationIsEmpty(1),
			box.AppendTrun(nil, 0, []uint32{4}, nil),
		))
	})
	rd, err := NewReader(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if rd.Info().FrameCount != len(audio) {
		t.Errorf("FrameCount = %d, want %d (a duration-is-empty traf carries no samples)", rd.Info().FrameCount, len(audio))
	}
	assertFramesEqual(t, drainFrames(t, rd), audio)
}

// TestFragmentDurationIsEmptyTrafStillValidated guards the other half of that
// early return: an empty fragment contributes no samples but is still the
// selected track's own box, so its runs are parsed and a malformed one fails.
func TestFragmentDurationIsEmptyTrafStillValidated(t *testing.T) {
	t.Parallel()
	stream, _ := fragmentedStreamWithExtraTraf(t, func(_ uint64) []byte {
		return handContainer("traf", concatBytes(
			handTfhdDurationIsEmpty(1),
			handTrunVersion(2, 1, 0),
		))
	})
	_, err := NewReader(bytes.NewReader(stream))
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("NewReader = %v, want ErrCorrupt", err)
	}
}
