// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"
)

// Box type names the fragmented tests match on, shared across the package's test
// files so the same literal is not repeated.
const (
	typeMoof = "moof"
	typeMdat = "mdat"
	typeOpus = "Opus"
)

// containerBoxes are the boxes walkBoxes descends into. stsd is handled
// separately because it is a FullBox with an entry_count before its children.
var containerBoxes = map[string]bool{
	"moov": true, "trak": true, "edts": true, "mdia": true, "minf": true,
	"stbl": true, "dinf": true, "mvex": true, typeMoof: true, "traf": true,
}

// walkBoxes indexes every box in data by its slash-separated path, mapping to the
// box body (the bytes after the header). It is the test-side counterpart of the
// writer: the tests assert on the emitted tree rather than on opaque byte blobs,
// so a failure names the box that is wrong.
func walkBoxes(t *testing.T, data []byte) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	var walk func(b []byte, prefix string)
	walk = func(b []byte, prefix string) {
		for off := 0; off+8 <= len(b); {
			size := int(binary.BigEndian.Uint32(b[off:]))
			typ := string(b[off+4 : off+8])
			hdr := 8
			if size == 1 {
				if off+16 > len(b) {
					t.Fatalf("truncated largesize header at %d in %q", off, prefix)
				}
				size = int(binary.BigEndian.Uint64(b[off+8:]))
				hdr = 16
			}
			if size < hdr || off+size > len(b) {
				t.Fatalf("box %q at %d has bad size %d (%d bytes left) in %q", typ, off, size, len(b)-off, prefix)
			}
			path := typ
			if prefix != "" {
				path = prefix + "/" + typ
			}
			body := b[off+hdr : off+size]
			out[path] = body
			switch {
			case containerBoxes[typ]:
				walk(body, path)
			case typ == "stsd":
				// FullBox(4) + entry_count(4), then the sample entries.
				walk(body[8:], path)
			}
			off += size
		}
	}
	walk(data, "")
	return out
}

func fullBoxVersionFlags(t *testing.T, body []byte) (version uint8, flags uint32) {
	t.Helper()
	if len(body) < 4 {
		t.Fatalf("FullBox body is %d bytes, want at least 4", len(body))
	}
	return body[0], uint32(body[1])<<16 | uint32(body[2])<<8 | uint32(body[3])
}

func mustBox(t *testing.T, tree map[string][]byte, path string) []byte {
	t.Helper()
	body, ok := tree[path]
	if !ok {
		paths := make([]string, 0, len(tree))
		for p := range tree {
			paths = append(paths, p)
		}
		t.Fatalf("box %q missing; present: %s", path, strings.Join(paths, " "))
	}
	return body
}

func aacFragmentConfig() WriterConfig {
	return WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
}

// topLevelBoxOffset returns the byte offset of the first top-level box of the
// given type, walking the box tree. Scanning for the four type bytes would be
// wrong: an mdat payload is arbitrary audio data and can contain any four-byte
// sequence, including a box type.
func topLevelBoxOffset(t *testing.T, data []byte, typ string) int {
	t.Helper()
	for off := 0; off+8 <= len(data); {
		size := int(binary.BigEndian.Uint32(data[off:]))
		if size == 1 {
			// The 64-bit largesize form, which a non-fragmented file's mdat uses.
			if off+16 > len(data) {
				t.Fatalf("truncated largesize header at offset %d while looking for %q", off, typ)
			}
			size = int(binary.BigEndian.Uint64(data[off+8:]))
		}
		if size < 8 || off+size > len(data) {
			t.Fatalf("malformed box at offset %d while looking for %q", off, typ)
		}
		if string(data[off+4:off+8]) == typ {
			return off
		}
		off += size
	}
	t.Fatalf("top-level box %q not found", typ)
	return 0
}

// TestInitSegmentStructure checks every field of the initialization segment that
// a player reads before it will accept a single media segment.
func TestInitSegmentStructure(t *testing.T) {
	init, err := InitSegment(aacFragmentConfig())
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	tree := walkBoxes(t, init)

	// ftyp must advertise cmfc (CMAF track-file conformance) and iso6, the
	// ISO-BMFF version fragmented-MP4 players expect to see listed.
	ftyp := mustBox(t, tree, "ftyp")
	if major := string(ftyp[0:4]); major != "cmfc" {
		t.Errorf("ftyp major brand = %q, want %q", major, "cmfc")
	}
	brands := make([]string, 0, (len(ftyp)-8)/4)
	for i := 8; i+4 <= len(ftyp); i += 4 {
		brands = append(brands, string(ftyp[i:i+4]))
	}
	for _, want := range []string{"cmfc", "iso6", "isom"} {
		if !slices.Contains(brands, want) {
			t.Errorf("ftyp compatible brands %v missing %q", brands, want)
		}
	}

	// Every duration is zero: a live stream has no known length.
	if got := binary.BigEndian.Uint32(mustBox(t, tree, "moov/mvhd")[16:]); got != 0 {
		t.Errorf("mvhd duration = %d, want 0", got)
	}
	if got := binary.BigEndian.Uint32(mustBox(t, tree, "moov/trak/tkhd")[20:]); got != 0 {
		t.Errorf("tkhd duration = %d, want 0", got)
	}
	mdhd := mustBox(t, tree, "moov/trak/mdia/mdhd")
	if got := binary.BigEndian.Uint32(mdhd[12:]); got != 48000 {
		t.Errorf("mdhd timescale = %d, want 48000", got)
	}
	if got := binary.BigEndian.Uint32(mdhd[16:]); got != 0 {
		t.Errorf("mdhd duration = %d, want 0", got)
	}

	// The edit list trims the AAC-LC priming, open-ended because the track's
	// duration is not yet known.
	elst := mustBox(t, tree, "moov/trak/edts/elst")
	if version, _ := fullBoxVersionFlags(t, elst); version != 0 {
		t.Errorf("elst version = %d, want 0", version)
	}
	if got := binary.BigEndian.Uint32(elst[4:]); got != 1 {
		t.Errorf("elst entry_count = %d, want 1", got)
	}
	if got := binary.BigEndian.Uint32(elst[8:]); got != 0 {
		t.Errorf("elst segment_duration = %d, want 0 (open ended)", got)
	}
	if got := int32(binary.BigEndian.Uint32(elst[12:])); got != DefaultEncoderDelay {
		t.Errorf("elst media_time = %d, want the AAC-LC priming %d", got, DefaultEncoderDelay)
	}

	// The sample tables must exist but hold nothing: the samples live in the
	// fragments, yet strict parsers refuse a track whose stbl lacks them.
	for _, tc := range []struct {
		path   string
		offset int
	}{
		{"moov/trak/mdia/minf/stbl/stts", 4},
		{"moov/trak/mdia/minf/stbl/stsc", 4},
		{"moov/trak/mdia/minf/stbl/stsz", 8}, // sample_size(4) then sample_count(4)
		{"moov/trak/mdia/minf/stbl/stco", 4},
	} {
		body := mustBox(t, tree, tc.path)
		if got := binary.BigEndian.Uint32(body[tc.offset:]); got != 0 {
			t.Errorf("%s entry count = %d, want 0", tc.path, got)
		}
	}
	if _, ok := tree["moov/trak/mdia/minf/stbl/stsd/mp4a"]; !ok {
		t.Error("stsd is missing the mp4a sample entry")
	}

	// mvex/trex is what declares the movie fragmented.
	trex := mustBox(t, tree, "moov/mvex/trex")
	if got := binary.BigEndian.Uint32(trex[4:]); got != fragmentTrackID {
		t.Errorf("trex track_ID = %d, want %d", got, fragmentTrackID)
	}
	if got := binary.BigEndian.Uint32(trex[12:]); got != samplesPerFrame {
		t.Errorf("trex default_sample_duration = %d, want %d", got, samplesPerFrame)
	}
	if got := binary.BigEndian.Uint32(trex[20:]); got != 0x02000000 {
		t.Errorf("trex default_sample_flags = %#08x, want 0x02000000 (sync sample)", got)
	}
}

// TestInitSegmentEditListVariants covers the three EncoderDelay behaviours the
// fragmented path inherits from the non-fragmented writer.
func TestInitSegmentEditListVariants(t *testing.T) {
	tests := []struct {
		name          string
		encoderDelay  int
		wantEdit      bool
		wantMediaTime int32
	}{
		{"default priming", 0, true, DefaultEncoderDelay},
		{"explicit priming", 2048, true, 2048},
		{"no edit list", NoEdit, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := aacFragmentConfig()
			cfg.EncoderDelay = tc.encoderDelay
			init, err := InitSegment(cfg)
			if err != nil {
				t.Fatalf("InitSegment: %v", err)
			}
			tree := walkBoxes(t, init)
			elst, ok := tree["moov/trak/edts/elst"]
			if ok != tc.wantEdit {
				t.Fatalf("edit list present = %v, want %v", ok, tc.wantEdit)
			}
			if !tc.wantEdit {
				return
			}
			if got := int32(binary.BigEndian.Uint32(elst[12:])); got != tc.wantMediaTime {
				t.Errorf("elst media_time = %d, want %d", got, tc.wantMediaTime)
			}
		})
	}
}

// TestInitSegmentNoEditListWithoutPriming covers the codec that has no priming to
// trim. An entry with media_time 0 and segment_duration 0 is a null edit, which a
// player reading segment_duration literally can take as "present nothing", so a
// zero trim must produce no edit list at all rather than a degenerate one. flacm4a
// suppresses it the same way on the non-fragmented path.
func TestInitSegmentNoEditListWithoutPriming(t *testing.T) {
	streamInfo := make([]byte, 34)
	tests := []struct {
		name     string
		cfg      WriterConfig
		wantEdit bool
	}{
		{"FLAC has no priming", WriterConfig{
			Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: streamInfo,
		}, false},
		{"FLAC with an explicit delay still trims", WriterConfig{
			Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: streamInfo,
			EncoderDelay: 128,
		}, true},
		{"AAC-LC primes by default", aacFragmentConfig(), true},
		{"Opus primes by default", WriterConfig{
			Codec: CodecOpus, SampleRate: 48000, Channels: 1,
		}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			init, err := InitSegment(tc.cfg)
			if err != nil {
				t.Fatalf("InitSegment: %v", err)
			}
			_, ok := walkBoxes(t, init)["moov/trak/edts/elst"]
			if ok != tc.wantEdit {
				t.Errorf("edit list present = %v, want %v", ok, tc.wantEdit)
			}
		})
	}
}

// TestInitSegmentBrandOverride covers the one WriterConfig field whose fragmented
// behaviour differs from the documented non-fragmented default.
func TestInitSegmentBrandOverride(t *testing.T) {
	cfg := aacFragmentConfig()
	cfg.Brand = "mp42"
	init, err := InitSegment(cfg)
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	ftyp := mustBox(t, walkBoxes(t, init), "ftyp")
	if major := string(ftyp[0:4]); major != "mp42" {
		t.Errorf("ftyp major brand = %q, want the override %q", major, "mp42")
	}
	// The compatible brands are fixed by the format and must survive an override,
	// so a player still sees the iso6 a fragmented segment has to declare.
	brands := make([]string, 0, (len(ftyp)-8)/4)
	for i := 8; i+4 <= len(ftyp); i += 4 {
		brands = append(brands, string(ftyp[i:i+4]))
	}
	if !slices.Contains(brands, "iso6") {
		t.Errorf("ftyp compatible brands %v lost iso6 to the override", brands)
	}

	// A media segment's styp is not the file brand, so an override must not reach
	// it: msdh/cmfs are what identify a CMAF media segment.
	f, err := NewFragmentWriter(cfg)
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	seg := segmentFrames(t, f, nil, synthFrames(2))
	styp := mustBox(t, walkBoxes(t, seg), "styp")
	if major := string(styp[0:4]); major != "msdh" {
		t.Errorf("styp major brand = %q, want %q; Brand must not reach a media segment", major, "msdh")
	}
}

// TestFragmentWriterSegmentCaps covers the two bounds that stop a caller who never
// flushes from growing the buffers without limit. Both must reject the offending
// write and leave the buffered samples flushable, rather than failing later at
// AppendSegment when the segment can no longer be emitted at all.
func TestFragmentWriterSegmentCaps(t *testing.T) {
	t.Run("byte cap", func(t *testing.T) {
		f, err := NewFragmentWriter(aacFragmentConfig())
		if err != nil {
			t.Fatalf("NewFragmentWriter: %v", err)
		}
		au := make([]byte, 1<<20) // 1 MiB per access unit
		var wrote int
		for range (maxSegmentBytes / len(au)) + 2 {
			if err := f.WriteFrame(au); err != nil {
				break
			}
			wrote++
		}
		if wrote == 0 {
			t.Fatal("the byte cap rejected the very first access unit")
		}
		if err := f.WriteFrame(au); err == nil {
			t.Fatalf("byte cap never fired after %d MiB", wrote)
		} else if !strings.Contains(err.Error(), "bytes") {
			t.Errorf("error %q does not mention the byte limit", err)
		}
		// The rejection must leave what was already buffered emittable.
		if _, err := f.AppendSegment(nil); err != nil {
			t.Errorf("AppendSegment after hitting the byte cap: %v", err)
		}
	})

	t.Run("sample cap", func(t *testing.T) {
		f, err := NewFragmentWriter(aacFragmentConfig())
		if err != nil {
			t.Fatalf("NewFragmentWriter: %v", err)
		}
		au := []byte{0xff}
		for range maxSamplesPerSegment {
			if err := f.WriteFrame(au); err != nil {
				t.Fatalf("WriteFrame below the cap: %v", err)
			}
		}
		if err := f.WriteFrame(au); err == nil {
			t.Fatal("sample cap never fired")
		} else if !strings.Contains(err.Error(), "samples") {
			t.Errorf("error %q does not mention the sample limit", err)
		}
		if _, err := f.AppendSegment(nil); err != nil {
			t.Errorf("AppendSegment after hitting the sample cap: %v", err)
		}
	})
}

// segmentFrames buffers n synthetic access units and flushes them as one segment.
func segmentFrames(t *testing.T, f *FragmentWriter, dst []byte, frames [][]byte) []byte {
	t.Helper()
	for _, au := range frames {
		if err := f.WriteFrame(au); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	out, err := f.AppendSegment(dst)
	if err != nil {
		t.Fatalf("AppendSegment: %v", err)
	}
	return out
}

// TestAppendSegmentStructure verifies a media segment's box tree, and above all
// that trun's data_offset lands exactly on the first byte of sample data. An
// offset that is wrong by even one byte makes a player decode garbage, and it is
// the one field computed rather than copied.
func TestAppendSegmentStructure(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	frames := synthFrames(5)
	seg := segmentFrames(t, f, nil, frames)

	tree := walkBoxes(t, seg)
	styp := mustBox(t, tree, "styp")
	if major := string(styp[0:4]); major != "msdh" {
		t.Errorf("styp major brand = %q, want %q", major, "msdh")
	}

	if got := binary.BigEndian.Uint32(mustBox(t, tree, "moof/mfhd")[4:]); got != 1 {
		t.Errorf("mfhd sequence_number = %d, want 1", got)
	}

	tfhd := mustBox(t, tree, "moof/traf/tfhd")
	_, flags := fullBoxVersionFlags(t, tfhd)
	if flags&0x020000 == 0 {
		t.Errorf("tfhd flags %#06x: default-base-is-moof must be set", flags)
	}
	if flags&0x000001 != 0 {
		t.Errorf("tfhd flags %#06x: base-data-offset-present is forbidden in CMAF", flags)
	}
	if flags&0x000008 == 0 {
		t.Fatalf("tfhd flags %#06x: uniform AAC-LC durations should use a default", flags)
	}
	if got := binary.BigEndian.Uint32(tfhd[8:]); got != samplesPerFrame {
		t.Errorf("tfhd default_sample_duration = %d, want %d", got, samplesPerFrame)
	}

	tfdt := mustBox(t, tree, "moof/traf/tfdt")
	if version, _ := fullBoxVersionFlags(t, tfdt); version != 1 {
		t.Errorf("tfdt version = %d, want 1 (64-bit, so a long stream cannot wrap)", version)
	}
	if got := binary.BigEndian.Uint64(tfdt[4:]); got != 0 {
		t.Errorf("tfdt baseMediaDecodeTime = %d, want 0 for the first segment", got)
	}

	// trun carries per-sample sizes but no durations, because tfhd supplied one.
	trun := mustBox(t, tree, "moof/traf/trun")
	_, trunFlags := fullBoxVersionFlags(t, trun)
	if trunFlags&0x000100 != 0 {
		t.Errorf("trun flags %#06x: per-sample durations are redundant with the tfhd default", trunFlags)
	}
	if trunFlags&0x000200 == 0 {
		t.Errorf("trun flags %#06x: sample sizes must be present", trunFlags)
	}
	if got := binary.BigEndian.Uint32(trun[4:]); got != uint32(len(frames)) {
		t.Errorf("trun sample_count = %d, want %d", got, len(frames))
	}
	for i := range frames {
		if got := binary.BigEndian.Uint32(trun[12+4*i:]); got != uint32(len(frames[i])) {
			t.Errorf("trun sample %d size = %d, want %d", i, got, len(frames[i]))
		}
	}

	// The decisive check: data_offset counts from the start of moof, so adding it
	// to moof's offset must land on the first sample byte inside mdat.
	dataOffset := int(int32(binary.BigEndian.Uint32(trun[8:])))
	// Locate the boxes by walking the tree, not by scanning for the type string:
	// mdat holds arbitrary sample bytes, which can contain "moof" or "mdat".
	moofStart := topLevelBoxOffset(t, seg, typeMoof)
	mdatStart := topLevelBoxOffset(t, seg, typeMdat)
	wantPayloadStart := mdatStart + 8
	if got := moofStart + dataOffset; got != wantPayloadStart {
		t.Fatalf("trun data_offset %d puts sample data at %d, want %d", dataOffset, got, wantPayloadStart)
	}

	// And the payload really is the access units, back to back.
	var wantPayload []byte
	for _, au := range frames {
		wantPayload = append(wantPayload, au...)
	}
	if got := seg[wantPayloadStart:]; !bytes.Equal(got, wantPayload) {
		t.Errorf("mdat payload = % x, want % x", got, wantPayload)
	}
}

// TestAppendSegmentAdvancesTimeline checks the two values that must be monotonic
// across a stream: the fragment sequence number and the decode time.
func TestAppendSegmentAdvancesTimeline(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}

	const segments, framesPerSegment = 4, 3
	var buf []byte
	for i := range segments {
		if got, want := f.BaseMediaDecodeTime(), uint64(i*framesPerSegment*samplesPerFrame); got != want {
			t.Fatalf("segment %d: BaseMediaDecodeTime = %d, want %d", i, got, want)
		}
		buf = segmentFrames(t, f, buf[:0], synthFrames(framesPerSegment))

		tree := walkBoxes(t, buf)
		if got, want := binary.BigEndian.Uint32(mustBox(t, tree, "moof/mfhd")[4:]), uint32(i+1); got != want {
			t.Errorf("segment %d: sequence_number = %d, want %d", i, got, want)
		}
		wantTime := uint64(i * framesPerSegment * samplesPerFrame)
		if got := binary.BigEndian.Uint64(mustBox(t, tree, "moof/traf/tfdt")[4:]); got != wantTime {
			t.Errorf("segment %d: baseMediaDecodeTime = %d, want %d", i, got, wantTime)
		}
	}
}

// TestPendingDurationTracksBuffer covers the value a caller cuts segments on and
// reports as EXTINF.
func TestPendingDurationTracksBuffer(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	if got := f.PendingDuration(); got != 0 {
		t.Errorf("PendingDuration on a fresh writer = %d, want 0", got)
	}
	for i, au := range synthFrames(3) {
		if err := f.WriteFrame(au); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
		if got, want := f.PendingDuration(), uint64((i+1)*samplesPerFrame); got != want {
			t.Errorf("after %d frames PendingDuration = %d, want %d", i+1, got, want)
		}
		if got, want := f.PendingSamples(), i+1; got != want {
			t.Errorf("PendingSamples = %d, want %d", got, want)
		}
	}
	if _, err := f.AppendSegment(nil); err != nil {
		t.Fatalf("AppendSegment: %v", err)
	}
	if got := f.PendingDuration(); got != 0 {
		t.Errorf("PendingDuration after a flush = %d, want 0", got)
	}
	if got := f.PendingSamples(); got != 0 {
		t.Errorf("PendingSamples after a flush = %d, want 0", got)
	}
}

// TestAppendSegmentVariableDurations covers the codec-generic path: when frames do
// not share a duration (FLAC's short final frame, a partial Opus packet) the
// per-sample durations move into trun.
func TestAppendSegmentVariableDurations(t *testing.T) {
	streamInfo := make([]byte, 34)
	f, err := NewFragmentWriter(WriterConfig{
		Codec: CodecFLAC, SampleRate: 44100, Channels: 2, STREAMINFO: streamInfo,
	})
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	durations := []uint32{4096, 4096, 1234}
	for i, d := range durations {
		if err := f.WriteFrameDuration([]byte{byte(i), 0xff, 0x00}, d); err != nil {
			t.Fatalf("WriteFrameDuration: %v", err)
		}
	}
	seg, err := f.AppendSegment(nil)
	if err != nil {
		t.Fatalf("AppendSegment: %v", err)
	}

	tree := walkBoxes(t, seg)
	_, tfhdFlags := fullBoxVersionFlags(t, mustBox(t, tree, "moof/traf/tfhd"))
	if tfhdFlags&0x000008 != 0 {
		t.Errorf("tfhd flags %#06x: no default duration can describe diverging frames", tfhdFlags)
	}
	trun := mustBox(t, tree, "moof/traf/trun")
	_, trunFlags := fullBoxVersionFlags(t, trun)
	if trunFlags&0x000100 == 0 {
		t.Fatalf("trun flags %#06x: per-sample durations must be present", trunFlags)
	}
	// Records are duration then size, 8 bytes each, after sample_count and data_offset.
	for i, want := range durations {
		if got := binary.BigEndian.Uint32(trun[12+8*i:]); got != want {
			t.Errorf("trun sample %d duration = %d, want %d", i, got, want)
		}
	}
	if got, want := f.BaseMediaDecodeTime(), uint64(4096+4096+1234); got != want {
		t.Errorf("BaseMediaDecodeTime = %d, want %d", got, want)
	}
}

// TestFragmentCodecSampleEntries checks the init segment carries the same
// codec-specific sample entry the non-fragmented writer emits.
func TestFragmentCodecSampleEntries(t *testing.T) {
	streamInfo := make([]byte, 34)
	tests := []struct {
		name  string
		cfg   WriterConfig
		entry string
		child string
	}{
		{"AAC-LC", aacFragmentConfig(), "mp4a", "esds"},
		{typeOpus, WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 2}, typeOpus, "dOps"},
		{"FLAC", WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: streamInfo}, "fLaC", "dfLa"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			init, err := InitSegment(tc.cfg)
			if err != nil {
				t.Fatalf("InitSegment: %v", err)
			}
			if !bytes.Contains(init, []byte(tc.entry)) {
				t.Errorf("init segment lacks the %s sample entry", tc.entry)
			}
			if !bytes.Contains(init, []byte(tc.child)) {
				t.Errorf("init segment lacks the %s codec box", tc.child)
			}

			// A codec whose frames vary in length declares no trex default duration.
			tree := walkBoxes(t, init)
			trex := mustBox(t, tree, "moov/mvex/trex")
			gotDefault := binary.BigEndian.Uint32(trex[12:])
			wantDefault := uint32(0)
			if tc.cfg.Codec == CodecAACLC {
				wantDefault = samplesPerFrame
			}
			if gotDefault != wantDefault {
				t.Errorf("trex default_sample_duration = %d, want %d", gotDefault, wantDefault)
			}
		})
	}
}

// TestFragmentWriterReset checks a pooled writer starts a new stream cleanly.
func TestFragmentWriterReset(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	_ = segmentFrames(t, f, nil, synthFrames(3))
	if f.BaseMediaDecodeTime() == 0 {
		t.Fatal("decode time did not advance")
	}
	// Buffer a frame that Reset must discard.
	if err := f.WriteFrame(synthFrames(1)[0]); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}

	if err := f.Reset(aacFragmentConfig()); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	if got := f.BaseMediaDecodeTime(); got != 0 {
		t.Errorf("BaseMediaDecodeTime after Reset = %d, want 0", got)
	}
	if got := f.PendingSamples(); got != 0 {
		t.Errorf("PendingSamples after Reset = %d, want 0", got)
	}
	seg := segmentFrames(t, f, nil, synthFrames(2))
	if got := binary.BigEndian.Uint32(mustBox(t, walkBoxes(t, seg), "moof/mfhd")[4:]); got != 1 {
		t.Errorf("sequence_number after Reset = %d, want 1", got)
	}

	// Reset also re-validates, so a bad config is refused.
	if err := f.Reset(WriterConfig{SampleRate: 48000, Channels: 9, ASC: ascMono48k}); err == nil {
		t.Error("Reset accepted an invalid channel count")
	}
}

// TestFragmentConfigRejectsMediaLength pins that the one WriterConfig field with
// no fragmented meaning is refused rather than quietly dropped.
func TestFragmentConfigRejectsMediaLength(t *testing.T) {
	cfg := aacFragmentConfig()
	cfg.MediaLength = 48000
	if _, err := InitSegment(cfg); err == nil {
		t.Error("InitSegment accepted MediaLength")
	} else if !strings.Contains(err.Error(), "MediaLength") {
		t.Errorf("error %q does not name MediaLength", err)
	}
	if _, err := NewFragmentWriter(cfg); err == nil {
		t.Error("NewFragmentWriter accepted MediaLength")
	}
}

// TestFragmentWriterErrors covers the rejected inputs and, critically, that a
// failed AppendSegment leaves both the caller's buffer and the writer untouched.
func TestFragmentWriterErrors(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}

	if err := f.WriteFrame(nil); err == nil {
		t.Error("WriteFrame accepted an empty access unit")
	}
	if err := f.WriteFrameDuration([]byte{1}, 0); err == nil {
		t.Error("WriteFrameDuration accepted a zero duration")
	}

	// Flushing with nothing buffered fails and must not disturb dst.
	dst := []byte("sentinel")
	out, err := f.AppendSegment(dst)
	if err == nil {
		t.Fatal("AppendSegment accepted an empty segment")
	}
	if !bytes.Equal(out, []byte("sentinel")) {
		t.Errorf("failed AppendSegment returned %q, want the input untouched", out)
	}

	// A codec with variable frame lengths has no fixed duration for WriteFrame.
	opus, err := NewFragmentWriter(WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 1})
	if err != nil {
		t.Fatalf("NewFragmentWriter(Opus): %v", err)
	}
	if err := opus.WriteFrame([]byte{1, 2, 3}); err == nil {
		t.Error("WriteFrame accepted an Opus frame; it should require WriteFrameDuration")
	} else if !strings.Contains(err.Error(), "WriteFrameDuration") {
		t.Errorf("error %q does not point at WriteFrameDuration", err)
	}

	// An invalid config is refused with the shared writer validation.
	if _, err := NewFragmentWriter(WriterConfig{SampleRate: 48000, Channels: 1, ASC: []byte{0x11}}); err == nil {
		t.Error("NewFragmentWriter accepted a truncated ASC")
	}
	if _, err := InitSegment(WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1}); err == nil {
		t.Error("InitSegment accepted FLAC with no STREAMINFO")
	}
}

// TestAppendSegmentAppendsToExistingBuffer checks the Append convention: existing
// content is preserved and the segment follows it.
func TestAppendSegmentAppendsToExistingBuffer(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	prefix := []byte("keep me")
	out := segmentFrames(t, f, prefix, synthFrames(2))
	if !bytes.HasPrefix(out, prefix) {
		t.Fatalf("AppendSegment overwrote the destination prefix: %q", out[:len(prefix)])
	}
	if !bytes.Contains(out[len(prefix):], []byte("moof")) {
		t.Error("segment was not appended after the prefix")
	}
}

// TestAppendSegmentSteadyStateAllocations is the requirement that motivated the
// accumulating API: one live stream emits a segment every couple of seconds on
// small ARM hardware, so once the buffers have grown a segment must allocate
// nothing at all.
func TestAppendSegmentSteadyStateAllocations(t *testing.T) {
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	frames := synthFrames(94) // about two seconds of AAC-LC at 48 kHz
	buf := make([]byte, 0, 64*1024)

	// Warm the buffers up so the measured runs are steady state.
	for range 3 {
		buf = segmentFrames(t, f, buf[:0], frames)
	}

	allocs := testing.AllocsPerRun(50, func() {
		for _, au := range frames {
			if err := f.WriteFrame(au); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
		}
		var err error
		buf, err = f.AppendSegment(buf[:0])
		if err != nil {
			t.Fatalf("AppendSegment: %v", err)
		}
	})
	if allocs != 0 {
		t.Errorf("steady-state segment allocates %.0f times, want 0", allocs)
	}
}

// TestFragmentedOutputIsRejectedByReader pins the documented split: this package
// writes fragmented output but does not read it, and the reader says so with a
// typed error rather than misparsing.
func TestFragmentedOutputIsRejectedByReader(t *testing.T) {
	init, err := InitSegment(aacFragmentConfig())
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	f, err := NewFragmentWriter(aacFragmentConfig())
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	stream := segmentFrames(t, f, init, synthFrames(4))

	tests := []struct {
		name string
		data []byte
	}{
		// The init segment on its own is the artefact an EXT-X-MAP serves, so it is
		// the one most likely to reach NewReader by mistake. It has no styp, so it
		// exercises the mvex rejection rather than the top-level scan.
		{"init segment (mvex)", init},
		{"media segment (styp)", stream[len(init):]},
		{"init and media segments", stream},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewReader(bytes.NewReader(tc.data)); !errors.Is(err, ErrUnsupported) {
				t.Fatalf("NewReader returned %v, want ErrUnsupported", err)
			}
		})
	}
}

// BenchmarkFragmentWriteFrame measures the per-access-unit hot path: one call per
// 1024 samples, about 47 times a second per live stream.
func BenchmarkFragmentWriteFrame(b *testing.B) {
	f, err := NewFragmentWriter(WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k})
	if err != nil {
		b.Fatalf("NewFragmentWriter: %v", err)
	}
	au := make([]byte, 384) // a typical 96 kbps AAC-LC access unit
	// Flush on the segment boundary a real packager would use, so the arena stays
	// at its steady-state size instead of growing towards the sample cap and
	// charging that growth to the per-frame cost.
	const framesPerSegment = 94
	var buf []byte
	pending := 0
	b.ReportAllocs()
	for b.Loop() {
		if err := f.WriteFrame(au); err != nil {
			b.Fatalf("WriteFrame: %v", err)
		}
		pending++
		if pending == framesPerSegment {
			var err error
			if buf, err = f.AppendSegment(buf[:0]); err != nil {
				b.Fatalf("AppendSegment: %v", err)
			}
			pending = 0
		}
	}
}

// BenchmarkAppendSegment measures one whole two-second segment, buffering and
// flushing, with the destination buffer reused as a live packager would reuse it.
// Steady state must stay at zero allocations.
func BenchmarkAppendSegment(b *testing.B) {
	f, err := NewFragmentWriter(WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k})
	if err != nil {
		b.Fatalf("NewFragmentWriter: %v", err)
	}
	const framesPerSegment = 94 // about two seconds of AAC-LC at 48 kHz
	au := make([]byte, 384)
	buf := make([]byte, 0, 64*1024)
	b.ReportAllocs()
	for b.Loop() {
		for range framesPerSegment {
			if err := f.WriteFrame(au); err != nil {
				b.Fatalf("WriteFrame: %v", err)
			}
		}
		if buf, err = f.AppendSegment(buf[:0]); err != nil {
			b.Fatalf("AppendSegment: %v", err)
		}
	}
}
