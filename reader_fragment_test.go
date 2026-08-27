// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
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
func buildFragmentedStream(t *testing.T, cfg WriterConfig, segments [][]fragAU) []byte {
	t.Helper()
	init, err := InitSegment(cfg)
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	fw, err := NewFragmentWriter(cfg)
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	out := append([]byte(nil), init...)
	for i, seg := range segments {
		for _, s := range seg {
			if err := fw.WriteFrameDuration(s.au, s.dur); err != nil {
				t.Fatalf("segment %d WriteFrameDuration: %v", i, err)
			}
		}
		out, err = fw.AppendSegment(out)
		if err != nil {
			t.Fatalf("segment %d AppendSegment: %v", i, err)
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
	init, err := InitSegment(aacFragmentConfig()) // audio track_ID is 1
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	audio := synthFrames(4)
	var audioBytes []byte
	audioSizes := make([]uint32, len(audio))
	audioDurs := make([]uint32, len(audio))
	for i, f := range audio {
		audioBytes = append(audioBytes, f...)
		audioSizes[i] = uint32(len(f))
		audioDurs[i] = 1024
	}
	foreignBytes := []byte("VIDEODATA-not-audio-and-must-be-skipped")
	foreignSizes := []uint32{uint32(len(foreignBytes))}

	base := int64(len(init)) // the moof starts here, once appended after the init

	// base_data_offset lives inside the moof but points past it into the mdat, so
	// build the moof once to measure its length (independent of the offset values),
	// then rebuild with the real absolute offsets.
	buildMoof := func(foreignBase, audioBase uint64) []byte {
		foreignTraf := handContainer("traf", concatBytes(
			handTfhd(2, foreignBase),
			box.AppendTfdt(nil, 0),
			box.AppendTrun(nil, 0, foreignSizes, nil),
		))
		audioTraf := handContainer("traf", concatBytes(
			handTfhd(1, audioBase),
			box.AppendTfdt(nil, 0),
			box.AppendTrun(nil, 0, audioSizes, audioDurs),
		))
		return handContainer("moof", concatBytes(box.AppendMfhd(nil, 1), foreignTraf, audioTraf))
	}
	moofLen := int64(len(buildMoof(0, 0)))
	mdatPayloadAbs := uint64(base + moofLen + box.MdatShortHeaderSize)
	foreignDataAbs := mdatPayloadAbs
	audioDataAbs := mdatPayloadAbs + uint64(len(foreignBytes))

	moof := buildMoof(foreignDataAbs, audioDataAbs)
	segment := concatBytes(moof, box.AppendMdat(nil, concatBytes(foreignBytes, audioBytes)))
	stream := concatBytes(init, segment)

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

// handTrunDataOffsetOnly builds a trun carrying only data_offset and sample_count,
// with no per-sample records, so every sample's size comes from the tfhd/trex
// default.
func handTrunDataOffsetOnly(sampleCount uint32, dataOffset int32) []byte {
	flags := uint32(0x000001) // data-offset-present only
	body := binary.BigEndian.AppendUint32(nil, sampleCount)
	body = binary.BigEndian.AppendUint32(body, uint32(dataOffset))
	return fragBox("trun", 0, flags, body)
}

// TestFragmentTfhdDefaultSampleSize exercises the resolveSampleSize fallback: the
// trun carries no per-sample sizes, so each sample's size comes from the tfhd
// default_sample_size. The writer never emits this shape (it always writes
// per-sample sizes), so it is hand-built.
func TestFragmentTfhdDefaultSampleSize(t *testing.T) {
	t.Parallel()
	init, err := InitSegment(aacFragmentConfig()) // audio track_ID 1
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	const n, sz = 4, 20
	payload := make([]byte, n*sz)
	for i := range payload {
		payload[i] = byte(i)
	}
	buildMoof := func(dataOffset int32) []byte {
		traf := handContainer("traf", concatBytes(
			handTfhdDefaultSize(1, sz),
			handTrunDataOffsetOnly(n, dataOffset),
		))
		return handContainer("moof", concatBytes(box.AppendMfhd(nil, 1), traf))
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
