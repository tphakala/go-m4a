// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// ascMono48k is a valid AAC-LC AudioSpecificConfig for 48000 Hz mono
// (audioObjectType 2, samplingFrequencyIndex 3, channelConfiguration 1).
var ascMono48k = []byte{0x11, 0x88}

// ascStereo44k is a valid AAC-LC AudioSpecificConfig for 44100 Hz stereo
// (audioObjectType 2, samplingFrequencyIndex 4, channelConfiguration 2).
var ascStereo44k = []byte{0x12, 0x10}

// memWS is an in-memory io.WriteSeeker backing the pure round-trip tests: it
// grows a byte slice on write and supports seeking anywhere, exactly what the
// Writer needs to patch the mdat size and append moov.
type memWS struct {
	buf []byte
	pos int64
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		grown := make([]byte, end)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

func (m *memWS) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	default:
		return 0, fmt.Errorf("memWS: bad whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("memWS: negative position %d", abs)
	}
	m.pos = abs
	return abs, nil
}

// synthFrames builds n synthetic access units of deliberately varied sizes so
// stsz and the mdat largesize exercise nonuniform lengths. The payload bytes are
// arbitrary; the container never decodes them.
func synthFrames(n int) [][]byte {
	frames := make([][]byte, n)
	for i := range frames {
		size := 7 + (i*13)%97 // 7..103 bytes, varied
		au := make([]byte, size)
		for j := range au {
			au[j] = byte(i*31 + j)
		}
		frames[i] = au
	}
	return frames
}

// writeM4A writes cfg + frames to a fresh memWS and returns the produced bytes.
func writeM4A(t *testing.T, cfg WriterConfig, frames [][]byte) []byte {
	t.Helper()
	ws := &memWS{}
	w, err := NewWriter(ws, cfg)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, au := range frames {
		if err := w.WriteFrame(au); err != nil {
			t.Fatalf("WriteFrame %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return ws.buf
}

// --- minimal box walk for assertions ---

// boxBody returns the body (bytes after the header) of the first top-level box
// named typ in data, handling both the 32-bit and 64-bit largesize headers.
func boxBody(t *testing.T, data []byte, typ string) []byte {
	t.Helper()
	body, _ := boxBodyAndSize(t, data, typ)
	return body
}

// boxBodyAndSize is boxBody plus the box's declared total size.
func boxBodyAndSize(t *testing.T, data []byte, typ string) (body []byte, declaredSize uint64) {
	t.Helper()
	for off := 0; off+8 <= len(data); {
		size := binary.BigEndian.Uint32(data[off:])
		hdr := 8
		boxLen := uint64(size)
		if size == 1 {
			if off+16 > len(data) {
				t.Fatalf("largesize box %q truncated at %d", typ, off)
			}
			boxLen = binary.BigEndian.Uint64(data[off+8:])
			hdr = 16
		}
		if boxLen < uint64(hdr) || off+int(boxLen) > len(data) {
			t.Fatalf("box at %d has bad size %d (remaining %d)", off, boxLen, len(data)-off)
		}
		if string(data[off+4:off+8]) == typ {
			return data[off+hdr : off+int(boxLen)], boxLen
		}
		off += int(boxLen)
	}
	t.Fatalf("box %q not found", typ)
	return nil, 0
}

// find descends a path of nested container boxes and returns the innermost
// box's body. Every box on the path must be a pure container (no fixed fields
// before its children), which holds for moov/trak/edts/mdia/minf/stbl.
func find(t *testing.T, data []byte, path ...string) []byte {
	t.Helper()
	for _, p := range path {
		data = boxBody(t, data, p)
	}
	return data
}

// hasBox reports whether data contains a top-level box named typ.
func hasBox(data []byte, typ string) bool {
	for off := 0; off+8 <= len(data); {
		size := binary.BigEndian.Uint32(data[off:])
		hdr := 8
		boxLen := uint64(size)
		if size == 1 {
			if off+16 > len(data) {
				return false
			}
			boxLen = binary.BigEndian.Uint64(data[off+8:])
			hdr = 16
		}
		if boxLen < uint64(hdr) || off+int(boxLen) > len(data) {
			return false
		}
		if string(data[off+4:off+8]) == typ {
			return true
		}
		off += int(boxLen)
	}
	return false
}

// fullBoxBody strips the 4-byte version/flags prefix of a FullBox body and
// returns the version and the remaining payload.
func fullBoxBody(body []byte) (version byte, payload []byte) {
	return body[0], body[4:]
}

func TestWriteAndReparse(t *testing.T) {
	const n = 5
	frames := synthFrames(n)
	var sum uint64
	sizes := make([]uint32, n)
	for i, f := range frames {
		sizes[i] = uint32(len(f))
		sum += uint64(len(f))
	}

	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
	data := writeM4A(t, cfg, frames)

	// ftyp: major brand "M4A ".
	ftyp := boxBody(t, data, "ftyp")
	if got := string(ftyp[:4]); got != "M4A " {
		t.Errorf("ftyp major brand = %q, want %q", got, "M4A ")
	}

	// mdat largesize == 16 + sum(sizes).
	_, mdatSize := boxBodyAndSize(t, data, "mdat")
	if want := 16 + sum; mdatSize != want {
		t.Errorf("mdat largesize = %d, want %d", mdatSize, want)
	}

	// stco offset == payloadStart == len(ftyp) + 16.
	wantOffset := uint32(len(ftyp) + 8 + 16) // ftyp body + its 8-byte header + mdat header
	stco := find(t, data, "moov", "trak", "mdia", "minf", "stbl", "stco")
	_, stcoPayload := fullBoxBody(stco)
	entryCount := binary.BigEndian.Uint32(stcoPayload)
	if entryCount != 1 {
		t.Fatalf("stco entry_count = %d, want 1", entryCount)
	}
	if got := binary.BigEndian.Uint32(stcoPayload[4:]); got != wantOffset {
		t.Errorf("stco offset = %d, want payloadStart %d", got, wantOffset)
	}

	// stsz entries == sizes.
	stsz := find(t, data, "moov", "trak", "mdia", "minf", "stbl", "stsz")
	_, stszPayload := fullBoxBody(stsz)
	if sampleSize := binary.BigEndian.Uint32(stszPayload); sampleSize != 0 {
		t.Errorf("stsz sample_size = %d, want 0", sampleSize)
	}
	if count := binary.BigEndian.Uint32(stszPayload[4:]); count != n {
		t.Fatalf("stsz sample_count = %d, want %d", count, n)
	}
	for i := 0; i < n; i++ {
		got := binary.BigEndian.Uint32(stszPayload[8+4*i:])
		if got != sizes[i] {
			t.Errorf("stsz[%d] = %d, want %d", i, got, sizes[i])
		}
	}

	// stts single run: (FrameCount, 1024).
	stts := find(t, data, "moov", "trak", "mdia", "minf", "stbl", "stts")
	_, sttsPayload := fullBoxBody(stts)
	if ec := binary.BigEndian.Uint32(sttsPayload); ec != 1 {
		t.Fatalf("stts entry_count = %d, want 1", ec)
	}
	if sc := binary.BigEndian.Uint32(sttsPayload[4:]); sc != n {
		t.Errorf("stts sample_count = %d, want %d", sc, n)
	}
	if sd := binary.BigEndian.Uint32(sttsPayload[8:]); sd != samplesPerFrame {
		t.Errorf("stts sample_delta = %d, want %d", sd, samplesPerFrame)
	}

	// Default edit list: media_time 1024, segment_duration N*1024 - 1024.
	seg, mt, ver := readElst(t, data)
	if ver != 0 {
		t.Errorf("elst version = %d, want 0", ver)
	}
	if mt != DefaultEncoderDelay {
		t.Errorf("elst media_time = %d, want %d", mt, DefaultEncoderDelay)
	}
	if want := uint64(n*samplesPerFrame - DefaultEncoderDelay); seg != want {
		t.Errorf("elst segment_duration = %d, want %d", seg, want)
	}

	// mvhd and tkhd durations both equal the presentation duration (segment).
	assertPresentationDuration(t, data, seg)
}

// readElst returns the segment_duration, media_time, and version of the elst.
func readElst(t *testing.T, data []byte) (segmentDuration uint64, mediaTime int64, version byte) {
	t.Helper()
	elst := find(t, data, "moov", "trak", "edts", "elst")
	ver, payload := fullBoxBody(elst)
	if ec := binary.BigEndian.Uint32(payload); ec != 1 {
		t.Fatalf("elst entry_count = %d, want 1", ec)
	}
	p := payload[4:]
	if ver == 1 {
		return binary.BigEndian.Uint64(p), int64(binary.BigEndian.Uint64(p[8:])), ver
	}
	return uint64(binary.BigEndian.Uint32(p)), int64(int32(binary.BigEndian.Uint32(p[4:]))), ver
}

// mvhdDuration reads the mvhd duration, honoring the version field.
func mvhdDuration(t *testing.T, data []byte) uint64 {
	t.Helper()
	ver, payload := fullBoxBody(boxBody(t, find(t, data, "moov"), "mvhd"))
	if ver == 1 {
		return binary.BigEndian.Uint64(payload[16:]) // skip 8+8 creation/modification
	}
	return uint64(binary.BigEndian.Uint32(payload[12:])) // skip 4+4 creation/mod + 4 timescale
}

// tkhdDuration reads the tkhd duration, honoring the version field.
func tkhdDuration(t *testing.T, data []byte) uint64 {
	t.Helper()
	ver, payload := fullBoxBody(boxBody(t, find(t, data, "moov", "trak"), "tkhd"))
	if ver == 1 {
		// creation(8) modification(8) track_ID(4) reserved(4) duration(8)
		return binary.BigEndian.Uint64(payload[24:])
	}
	// creation(4) modification(4) track_ID(4) reserved(4) duration(4)
	return uint64(binary.BigEndian.Uint32(payload[16:]))
}

func assertPresentationDuration(t *testing.T, data []byte, want uint64) {
	t.Helper()
	if got := mvhdDuration(t, data); got != want {
		t.Errorf("mvhd duration = %d, want %d", got, want)
	}
	if got := tkhdDuration(t, data); got != want {
		t.Errorf("tkhd duration = %d, want %d", got, want)
	}
}

func TestEditListVariants(t *testing.T) {
	const n = 10 // 10240 media samples
	frames := synthFrames(n)

	tests := []struct {
		name        string
		cfg         WriterConfig
		wantEdit    bool
		wantMedia   int64  // expected elst media_time
		wantSegment uint64 // expected elst segment_duration and presentation duration
	}{
		{
			name:        "default delay",
			cfg:         WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k},
			wantEdit:    true,
			wantMedia:   DefaultEncoderDelay,
			wantSegment: n*samplesPerFrame - DefaultEncoderDelay,
		},
		{
			name:        "explicit delay",
			cfg:         WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, EncoderDelay: 2112},
			wantEdit:    true,
			wantMedia:   2112,
			wantSegment: n*samplesPerFrame - 2112,
		},
		{
			name:        "media length set",
			cfg:         WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, MediaLength: 5000},
			wantEdit:    true,
			wantMedia:   DefaultEncoderDelay,
			wantSegment: 5000,
		},
		{
			name:        "media length zero explicit",
			cfg:         WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, MediaLength: 0},
			wantEdit:    true,
			wantMedia:   DefaultEncoderDelay,
			wantSegment: n*samplesPerFrame - DefaultEncoderDelay,
		},
		{
			name:     "no edit",
			cfg:      WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, EncoderDelay: NoEdit},
			wantEdit: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := writeM4A(t, tc.cfg, frames)

			if !tc.wantEdit {
				if hasEdts(t, data) {
					t.Error("edts present, want none")
				}
				assertPresentationDuration(t, data, n*samplesPerFrame)
				return
			}

			seg, mt, _ := readElst(t, data)
			if mt != tc.wantMedia {
				t.Errorf("media_time = %d, want %d", mt, tc.wantMedia)
			}
			if seg != tc.wantSegment {
				t.Errorf("segment_duration = %d, want %d", seg, tc.wantSegment)
			}
			assertPresentationDuration(t, data, tc.wantSegment)
		})
	}
}

// hasEdts reports whether the trak contains an edts box.
func hasEdts(t *testing.T, data []byte) bool {
	t.Helper()
	return hasBox(find(t, data, "moov", "trak"), "edts")
}

func TestNewWriterErrors(t *testing.T) {
	tests := []struct {
		name string
		cfg  WriterConfig
	}{
		{"rate mismatch", WriterConfig{SampleRate: 44100, Channels: 1, ASC: ascMono48k}},
		{"channel mismatch", WriterConfig{SampleRate: 48000, Channels: 2, ASC: ascMono48k}},
		{"bad channels high", WriterConfig{SampleRate: 48000, Channels: 3, ASC: ascMono48k}},
		{"bad channels zero", WriterConfig{SampleRate: 48000, Channels: 0, ASC: ascMono48k}},
		{"asc too short", WriterConfig{SampleRate: 48000, Channels: 1, ASC: []byte{0x11}}},
		{"unsupported rate", WriterConfig{SampleRate: 12345, Channels: 1, ASC: ascMono48k}},
		{"negative media length", WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, MediaLength: -1}},
		{"invalid encoder delay", WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, EncoderDelay: -2}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewWriter(&memWS{}, tc.cfg); err == nil {
				t.Fatal("NewWriter succeeded, want error")
			}
		})
	}
}

func TestNewWriterNilWriter(t *testing.T) {
	if _, err := NewWriter(nil, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}); err == nil {
		t.Fatal("NewWriter(nil) succeeded, want error")
	}
}

func TestWriteFrameRejectsEmpty(t *testing.T) {
	w, err := NewWriter(&memWS{}, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteFrame(nil); err == nil {
		t.Error("WriteFrame(nil) succeeded, want error")
	}
	if err := w.WriteFrame([]byte{}); err == nil {
		t.Error("WriteFrame(empty) succeeded, want error")
	}
}

func TestUseAfterClose(t *testing.T) {
	w, err := NewWriter(&memWS{}, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteFrame([]byte{1, 2, 3}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := w.WriteFrame([]byte{4, 5, 6}); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteFrame after Close = %v, want ErrClosed", err)
	}
	if err := w.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close = %v, want ErrClosed", err)
	}
}

func TestCloseWithoutFrames(t *testing.T) {
	w, err := NewWriter(&memWS{}, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.Close(); err == nil {
		t.Fatal("Close with no frames succeeded, want error")
	}
}

func TestStereo44kReparse(t *testing.T) {
	frames := synthFrames(3)
	data := writeM4A(t, WriterConfig{SampleRate: 44100, Channels: 2, ASC: ascStereo44k}, frames)

	// mp4a channelcount and samplerate live in the fixed AudioSampleEntry area.
	stsd := find(t, data, "moov", "trak", "mdia", "minf", "stbl", "stsd")
	// stsd: version/flags(4) entry_count(4), then the mp4a box.
	mp4a := stsd[8:]
	// mp4a body starts after its 8-byte header; channelcount is at body offset 16.
	body := mp4a[8:]
	if ch := binary.BigEndian.Uint16(body[16:]); ch != 2 {
		t.Errorf("mp4a channelcount = %d, want 2", ch)
	}
	if sr := binary.BigEndian.Uint32(body[24:]) >> 16; sr != 44100 {
		t.Errorf("mp4a samplerate = %d, want 44100", sr)
	}
}

// countingWS is a byte-counting io.WriteSeeker that keeps only the first
// headerCap bytes and discards the rest. It lets the co64-scale test simulate a
// multi-gigabyte mdat and still inspect the patched header without allocating
// gigabytes of memory.
type countingWS struct {
	header [headerCap]byte
	length int64 // highest byte offset written
	pos    int64
}

const headerCap = 512

func (c *countingWS) Write(p []byte) (int, error) {
	end := c.pos + int64(len(p))
	// Capture any portion of this write that lands within the retained header.
	if c.pos < headerCap {
		hi := end
		if hi > headerCap {
			hi = headerCap
		}
		copy(c.header[c.pos:hi], p[:hi-c.pos])
	}
	c.pos = end
	if end > c.length {
		c.length = end
	}
	return len(p), nil
}

func (c *countingWS) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = c.pos + offset
	case io.SeekEnd:
		abs = c.length + offset
	default:
		return 0, fmt.Errorf("countingWS: bad whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("countingWS: negative position %d", abs)
	}
	c.pos = abs
	return abs, nil
}

func TestCo64ScaleLargesizePatch(t *testing.T) {
	ws := &countingWS{}
	w, err := NewWriter(ws, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Push the mdat payload past 4 GiB by reusing one 1 MiB buffer, so the
	// simulated file is huge but memory use stays at a single megabyte.
	const chunk = 1 << 20 // 1 MiB
	au := make([]byte, chunk)
	iterations := int64(1<<32/chunk) + 8 // a little over 4 GiB
	var wantPayload int64
	for i := int64(0); i < iterations; i++ {
		if err := w.WriteFrame(au); err != nil {
			t.Fatalf("WriteFrame %d: %v", i, err)
		}
		wantPayload += chunk
	}
	if wantPayload <= 1<<32 {
		t.Fatalf("test setup: payload %d did not exceed 4 GiB", wantPayload)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The retained header holds ftyp then the mdat 64-bit largesize header. The
	// patched largesize sits at the mdat box offset + 8.
	_, ftypSize := boxBodyAndSize(t, ws.header[:], "ftyp")
	mdatOff := int64(ftypSize)
	largesize := binary.BigEndian.Uint64(ws.header[mdatOff+8:])
	want := uint64(16 + wantPayload)
	if largesize != want {
		t.Errorf("patched mdat largesize = %d, want %d", largesize, want)
	}
	if largesize <= 1<<32 {
		t.Errorf("largesize %d did not exceed uint32 range; 64-bit path untested", largesize)
	}
}

func TestFFprobeInterop(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found on PATH")
	}

	const n = 8
	frames := synthFrames(n)
	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
	data := writeM4A(t, cfg, frames)

	path := filepath.Join(t.TempDir(), "synth.m4a")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	out, err := exec.Command(ffprobe,
		"-v", "error",
		"-show_format", "-show_streams", "-show_packets", "-count_packets",
		"-print_format", "json", path).Output()
	if err != nil {
		t.Fatalf("ffprobe failed: %v\n%s", err, out)
	}

	var probe struct {
		Streams []struct {
			CodecName     string `json:"codec_name"`
			SampleRate    string `json:"sample_rate"`
			Channels      int    `json:"channels"`
			NbReadPackets string `json:"nb_read_packets"`
		} `json:"streams"`
		Packets []json.RawMessage `json:"packets"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("parse ffprobe json: %v\n%s", err, out)
	}
	if len(probe.Streams) == 0 {
		t.Fatalf("ffprobe reported no streams:\n%s", out)
	}
	s := probe.Streams[0]
	if s.CodecName != "aac" {
		t.Errorf("codec_name = %q, want aac", s.CodecName)
	}
	if s.SampleRate != "48000" {
		t.Errorf("sample_rate = %q, want 48000", s.SampleRate)
	}
	if s.Channels != 1 {
		t.Errorf("channels = %d, want 1", s.Channels)
	}
	if len(probe.Packets) != n {
		t.Errorf("packets array length = %d, want %d", len(probe.Packets), n)
	}
	if s.NbReadPackets != fmt.Sprint(n) {
		t.Errorf("nb_read_packets = %q, want %d", s.NbReadPackets, n)
	}
	t.Logf("ffprobe: codec_name=%s sample_rate=%s channels=%d nb_read_packets=%s packets=%d",
		s.CodecName, s.SampleRate, s.Channels, s.NbReadPackets, len(probe.Packets))
}

// sanity: the reparse helpers agree with a direct scan that the file starts with
// ftyp followed immediately by mdat.
func TestTopLevelBoxOrder(t *testing.T) {
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, synthFrames(2))
	if got := string(data[4:8]); got != "ftyp" {
		t.Fatalf("first box = %q, want ftyp", got)
	}
	_, ftypSize := boxBodyAndSize(t, data, "ftyp")
	if got := string(data[int(ftypSize)+4 : int(ftypSize)+8]); got != "mdat" {
		t.Fatalf("second box = %q, want mdat", got)
	}
	if !bytes.Contains(data, []byte("moov")) {
		t.Fatal("moov box missing")
	}
}
