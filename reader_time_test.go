// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/go-m4a/internal/box"
)

// ascMono16k and ascMono44k are valid AAC-LC AudioSpecificConfigs for mono at
// 16000 Hz (samplingFrequencyIndex 8) and 44100 Hz (index 4). 16000 Hz makes a
// tick exactly 62500 ns, so the time-to-tick conversion in the sweeps is exact.
var (
	ascMono16k = []byte{0x14, 0x08}
	ascMono44k = []byte{0x12, 0x08}
)

// writeTimed writes one tiny access unit per entry of durs through the Writer
// with the given per-unit durations, so the stts has as many runs as the list has
// maximal equal-duration stretches.
func writeTimed(t *testing.T, cfg WriterConfig, durs []uint32) []byte {
	t.Helper()
	ws := &memWS{}
	w, err := NewWriter(ws, cfg)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, d := range durs {
		if err := w.WriteFrameDuration([]byte{byte(i), byte(i >> 8), 1}, d); err != nil {
			t.Fatalf("WriteFrameDuration %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return ws.buf
}

// tickOracle maps a media tick to the (unit, offset) pair whose duration span
// contains it, skipping zero-duration units, by direct summation. ok is false
// when tick is at or past the total.
func tickOracle(durs []uint32, tick uint64) (sample int, off int64, ok bool) {
	var acc uint64
	for i, d := range durs {
		if tick < acc+uint64(d) {
			return i, int64(tick - acc), true
		}
		acc += uint64(d)
	}
	return 0, 0, false
}

func sumDurs(durs []uint32) uint64 {
	var n uint64
	for _, d := range durs {
		n += uint64(d)
	}
	return n
}

// tickToDuration is the exact inverse of the reader's round-to-nearest
// conversion for the timescales used here (the error is far below half a tick).
func tickToDuration(tick uint64, ts uint32) time.Duration {
	return time.Duration((tick*1e9 + uint64(ts)/2) / uint64(ts))
}

// sweepTicks checks SeekTime against tickOracle for every tick in
// [0, presented) and ErrOutOfRange for the few ticks after, and that the result
// is monotone (I5). origin is the media tick that presentation time 0 maps to.
func sweepTicks(t *testing.T, r *Reader, durs []uint32, ts uint32, origin, presentEnd uint64) {
	t.Helper()
	var prevS int
	var prevK int64 = -1
	for tick := range presentEnd - origin {
		d := tickToDuration(tick, ts)
		s, k, err := r.SeekTime(d)
		ws, wk, ok := tickOracle(durs, origin+tick)
		if !ok {
			t.Fatalf("test bug: oracle has no unit for tick %d", origin+tick)
		}
		if err != nil || s != ws || k != wk {
			t.Fatalf("SeekTime(tick %d = %v) = (%d, %d, %v), want (%d, %d, nil)", tick, d, s, k, err, ws, wk)
		}
		if s < prevS || (s == prevS && k < prevK) {
			t.Fatalf("SeekTime not monotone at tick %d: (%d,%d) after (%d,%d)", tick, s, k, prevS, prevK)
		}
		prevS, prevK = s, k
	}
	for tick := presentEnd - origin; tick < presentEnd-origin+3; tick++ {
		s, k, err := r.SeekTime(tickToDuration(tick, ts))
		if !errors.Is(err, ErrOutOfRange) || s != 0 || k != 0 {
			t.Fatalf("SeekTime(tick %d) = (%d, %d, %v), want (0, 0, ErrOutOfRange)", tick, s, k, err)
		}
	}
}

func TestSeekTimeInteropFixtures(t *testing.T) {
	type probe struct {
		d      time.Duration
		sample int
		skip   int64
		oor    bool
	}
	cases := []struct {
		name   string
		timing int // expected number of timing runs
		probes []probe
	}{
		{"ffmpeg_mono48k.m4a", 2, []probe{
			{d: 0, sample: 1, skip: 0},
			{d: 10 * time.Millisecond, sample: 1, skip: 480},
			{d: 600*time.Millisecond - 20833, sample: 29, skip: 127},
			{d: 600 * time.Millisecond, oor: true},
		}},
		{"ffmpeg_stereo44k.m4a", 2, []probe{
			{d: 0, sample: 1, skip: 0},
			{d: 500 * time.Millisecond, sample: 22, skip: 546},
			{d: 600 * time.Millisecond, oor: true},
		}},
		{"opus_mono48k.mp4", 2, []probe{
			{d: 0, sample: 0, skip: 312},
			{d: 13500 * time.Microsecond, sample: 1, skip: 0},
			{d: time.Second - 20833, sample: 50, skip: 311},
			{d: time.Second, oor: true},
		}},
		{"flac_mono44k.mp4", 2, []probe{
			{d: 0, sample: 0, skip: 0},
			// 9 * 4608 ticks at 44100 Hz is 940408163.27 ns; the truncated value is
			// 0.27 ns short of the tick, which only round-to-nearest maps onto unit 9.
			{d: 940408163, sample: 9, skip: 0},
			{d: time.Second, oor: true},
		}},
		{"afconvert_mono48k.m4a", 1, []probe{
			{d: 0, sample: 0, skip: 0},
			{d: -1, oor: true},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "interop", tc.name))
			if err != nil {
				t.Fatalf("read fixture: %v", err)
			}
			r, err := NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			if len(r.timeRuns) != tc.timing {
				t.Errorf("timing runs = %d, want %d", len(r.timeRuns), tc.timing)
			}
			ref, err := NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			want := drainFrames(t, ref)
			probes := tc.probes
			if tc.name == "afconvert_mono48k.m4a" {
				// The no-elst end is the media end: Info().Duration is the last
				// presented instant and must be refused.
				probes = append(probes, probe{d: r.Info().Duration, oor: true})
			}
			for _, p := range probes {
				s, k, err := r.SeekTime(p.d)
				if p.oor {
					if !errors.Is(err, ErrOutOfRange) || s != 0 || k != 0 {
						t.Errorf("SeekTime(%v) = (%d, %d, %v), want (0, 0, ErrOutOfRange)", p.d, s, k, err)
					}
					continue
				}
				if err != nil || s != p.sample || k != p.skip {
					t.Errorf("SeekTime(%v) = (%d, %d, %v), want (%d, %d, nil)", p.d, s, k, err, p.sample, p.skip)
					continue
				}
				got, err := r.ReadFrame()
				if err != nil || !bytes.Equal(got, want[s]) {
					t.Errorf("SeekTime(%v): ReadFrame differs from sequential unit %d (err %v)", p.d, s, err)
				}
			}
		})
	}
}

func TestSeekTimeMatchesTickOracle(t *testing.T) {
	durs := []uint32{1024, 1024, 512, 2048, 7, 1024, 1024, 300}
	total := sumDurs(durs)
	type cfgCase struct {
		name   string
		rate   int
		asc    []byte
		delay  int
		mlen   int64
		origin uint64
		end    uint64 // presentEnd in media ticks
	}
	cases := make([]cfgCase, 0, 9)
	for _, dly := range []int{NoEdit, 1, 1024, 1500} {
		for _, extra := range []int64{0, 1} { // 1: MediaLength = total - delay - 200
			origin := uint64(0)
			if dly > 0 {
				origin = uint64(dly)
			}
			var mlen int64
			end := total
			if extra == 1 && dly != NoEdit {
				mlen = int64(total-origin) - 200
				end = origin + uint64(mlen)
			}
			cases = append(cases, cfgCase{"16k", 16000, ascMono16k, dly, mlen, origin, end})
		}
	}
	cases = append(cases, cfgCase{"44k", 44100, ascMono44k, 1500, 0, 1500, total})
	for _, c := range cases {
		cfg := WriterConfig{SampleRate: c.rate, Channels: 1, ASC: c.asc, EncoderDelay: c.delay, MediaLength: c.mlen}
		data := writeTimed(t, cfg, durs)
		r, err := NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s delay=%d mlen=%d: NewReader: %v", c.name, c.delay, c.mlen, err)
		}
		if got := uint64(r.Info().EncoderDelay); got != c.origin {
			t.Fatalf("%s delay=%d: EncoderDelay = %d, want %d", c.name, c.delay, got, c.origin)
		}
		t.Run(c.name, func(t *testing.T) {
			sweepTicks(t, r, durs, uint32(c.rate), c.origin, c.end)
		})
	}
}

func TestSeekTimeFragmentedMixedDurations(t *testing.T) {
	mk := func(durs ...uint32) []fragAU {
		seg := make([]fragAU, len(durs))
		for i, d := range durs {
			seg[i] = fragAU{au: []byte{byte(i), 9, byte(d)}, dur: d}
		}
		return seg
	}
	segments := [][]fragAU{mk(1024, 1024, 1024), mk(512, 2048), mk(1024), mk(7, 1024)}
	var durs []uint32
	for _, s := range segments {
		for _, u := range s {
			durs = append(durs, u.dur)
		}
	}
	for _, tc := range []struct {
		name   string
		cfg    WriterConfig
		origin uint64
		runs   int
	}{
		{"aac", aacFragmentConfig(), 1024, 6},
		{"opus", WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 1, OpusPreSkip: 312}, 312, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewReader(bytes.NewReader(buildFragmentedStream(t, tc.cfg, segments)))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			if len(r.timeRuns) != tc.runs {
				t.Errorf("timing runs = %d, want %d (maximal equal-duration runs)", len(r.timeRuns), tc.runs)
			}
			if got := uint64(r.Info().EncoderDelay); got != tc.origin {
				t.Fatalf("EncoderDelay = %d, want %d", got, tc.origin)
			}
			sweepTicks(t, r, durs, 48000, tc.origin, sumDurs(durs))
		})
	}
}

func TestSeekTimePrimingBoundary(t *testing.T) {
	durs := make([]uint32, 6)
	for i := range durs {
		durs[i] = 1024
	}
	for _, tc := range []struct {
		delay  int
		sample int
		skip   int64
	}{{1023, 0, 1023}, {1024, 1, 0}, {1025, 1, 1}, {2048, 2, 0}} {
		data := writeTimed(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, EncoderDelay: tc.delay}, durs)
		r, err := NewReader(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		s, k, err := r.SeekTime(0)
		if err != nil || s != tc.sample || k != tc.skip {
			t.Errorf("delay %d: SeekTime(0) = (%d, %d, %v), want (%d, %d, nil)", tc.delay, s, k, err, tc.sample, tc.skip)
		}
	}
}

func TestSeekTimeOutOfRange(t *testing.T) {
	durs := []uint32{1024, 1024, 1024, 1024, 1024, 1024}
	data := writeTimed(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, durs)
	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	want := drainFrames(t, r)
	if err := r.SeekSample(3); err != nil {
		t.Fatalf("SeekSample: %v", err)
	}
	for _, d := range []time.Duration{-1, math.MinInt64, r.Info().Duration, math.MaxInt64} {
		s, k, err := r.SeekTime(d)
		if !errors.Is(err, ErrOutOfRange) || errors.Is(err, ErrCorrupt) || s != 0 || k != 0 {
			t.Errorf("SeekTime(%d) = (%d, %d, %v), want (0, 0, ErrOutOfRange)", d, s, k, err)
		}
		got, err := r.ReadFrame()
		if err != nil || !bytes.Equal(got, want[3]) {
			t.Fatalf("after SeekTime(%d) the cursor moved (err %v)", d, err)
		}
		if err := r.SeekSample(3); err != nil {
			t.Fatalf("SeekSample: %v", err)
		}
	}
}

func TestSeekTimeEndExcludesPadding(t *testing.T) {
	durs := []uint32{1024, 1024, 1024, 1024}
	const ts = 16000
	cfg := WriterConfig{SampleRate: ts, Channels: 1, ASC: ascMono16k, EncoderDelay: 100, MediaLength: 3000}
	r, err := NewReader(bytes.NewReader(writeTimed(t, cfg, durs)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	// Presented ticks are [0, 3000); media ticks [100, 3100) of 4096.
	s, k, err := r.SeekTime(tickToDuration(2999, ts))
	if ws, wk, _ := tickOracle(durs, 3099); err != nil || s != ws || k != wk {
		t.Errorf("last presented tick = (%d, %d, %v), want (%d, %d, nil)", s, k, err, ws, wk)
	}
	if _, _, err := r.SeekTime(tickToDuration(3000, ts)); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("first padding tick err = %v, want ErrOutOfRange", err)
	}

	// A MediaLength beyond the media (the Writer accepts it) clamps to the media end.
	cfg.MediaLength = 100000
	r, err = NewReader(bytes.NewReader(writeTimed(t, cfg, durs)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	sweepTicks(t, r, durs, ts, 100, 4096)
}

// patchSttsBody runs fn over data's stts FullBox body (version/flags onward), found
// inside the moov so mdat payload bytes cannot match.
func patchSttsBody(t *testing.T, data []byte, fn func(body []byte)) {
	t.Helper()
	moov := topLevelBoxOffset(t, data, "moov")
	i := bytes.Index(data[moov:], []byte("stts"))
	if i < 0 {
		t.Fatal("no stts in moov")
	}
	fn(data[moov+i+4:])
}

func TestSeekTimeNoTiming(t *testing.T) {
	durs := []uint32{1024, 1024, 1024}
	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
	for _, tc := range []struct {
		name  string
		patch func(t *testing.T, data []byte)
	}{
		{"stts renamed", func(t *testing.T, data []byte) {
			t.Helper()
			// An unknown child type is ignored by the stbl walk, so the file opens
			// with no timing table.
			moov := topLevelBoxOffset(t, data, "moov")
			i := bytes.Index(data[moov:], []byte("stts"))
			if i < 0 {
				t.Fatal("no stts in moov")
			}
			copy(data[moov+i:], "sttx")
		}},
		{"zero deltas", func(t *testing.T, data []byte) {
			t.Helper()
			patchSttsBody(t, data, func(body []byte) {
				for i := range int(binary.BigEndian.Uint32(body[4:])) {
					binary.BigEndian.PutUint32(body[8+8*i+4:], 0)
				}
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := writeTimed(t, cfg, durs)
			tc.patch(t, data)
			r, err := NewReader(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			if err := r.SeekSample(2); err != nil {
				t.Fatal(err)
			}
			s, k, err := r.SeekTime(0)
			if !errors.Is(err, ErrUnsupported) || s != 0 || k != 0 {
				t.Fatalf("SeekTime = (%d, %d, %v), want (0, 0, ErrUnsupported)", s, k, err)
			}
			if _, err := r.ReadFrame(); err != nil {
				t.Fatalf("ReadFrame after rejected SeekTime: %v", err)
			}
			if _, err := r.ReadFrame(); !errors.Is(err, io.EOF) {
				t.Fatalf("cursor was not at unit 2: second read err = %v, want EOF", err)
			}
		})
	}
}

func TestSeekTimeSkipsZeroDurationUnit(t *testing.T) {
	cfg := WriterConfig{SampleRate: 16000, Channels: 1, ASC: ascMono16k, EncoderDelay: NoEdit}
	data := writeTimed(t, cfg, []uint32{1024, 7, 1024})
	patchSttsBody(t, data, func(body []byte) {
		if n := binary.BigEndian.Uint32(body[4:]); n != 3 {
			t.Fatalf("stts has %d runs, want 3", n)
		}
		binary.BigEndian.PutUint32(body[8+8+4:], 0) // second run's delta
	})
	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	s, k, err := r.SeekTime(tickToDuration(1024, 16000))
	if err != nil || s != 2 || k != 0 {
		t.Fatalf("SeekTime(tick 1024) = (%d, %d, %v), want (2, 0, nil)", s, k, err)
	}
}

func TestSeekTimeLeadingEmptyEdit(t *testing.T) {
	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
	data := writeTimed(t, cfg, []uint32{1024, 1024, 1024})
	moov := topLevelBoxOffset(t, data, "moov")
	i := bytes.Index(data[moov:], []byte("elst"))
	if i < 0 {
		t.Fatal("no elst in moov")
	}
	body := data[moov+i+4:]
	if body[0] != 0 {
		t.Fatalf("elst version %d, test expects 0", body[0])
	}
	binary.BigEndian.PutUint32(body[8+4:], 0xFFFFFFFF) // first entry media_time = -1
	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	s, k, err := r.SeekTime(0)
	if !errors.Is(err, ErrUnsupported) || s != 0 || k != 0 {
		t.Fatalf("SeekTime = (%d, %d, %v), want (0, 0, ErrUnsupported)", s, k, err)
	}
}

func TestSeekTimeIdempotent(t *testing.T) {
	durs := []uint32{1024, 1024, 512, 1024, 1024}
	cfg := WriterConfig{SampleRate: 16000, Channels: 1, ASC: ascMono16k, EncoderDelay: 100}
	r, err := NewReader(bytes.NewReader(writeTimed(t, cfg, durs)))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	want := drainFrames(t, r)
	d := tickToDuration(2000, 16000)
	ws, wk, _ := tickOracle(durs, 2100)
	check := func(when string) {
		t.Helper()
		s, k, err := r.SeekTime(d)
		if err != nil || s != ws || k != wk {
			t.Fatalf("%s: SeekTime = (%d, %d, %v), want (%d, %d, nil)", when, s, k, err, ws, wk)
		}
		got, err := r.ReadFrame()
		if err != nil || !bytes.Equal(got, want[ws]) {
			t.Fatalf("%s: ReadFrame returned the wrong unit (err %v)", when, err)
		}
	}
	check("after drain to EOF")
	check("repeat")
	if err := r.SeekSample(0); err != nil {
		t.Fatal(err)
	}
	check("after SeekSample(0)")
	drainFrames(t, r)
	check("after second drain")
}

func bigTicks(d int64, ts uint32) (*big.Int, bool) {
	n := new(big.Int).Mul(big.NewInt(d), big.NewInt(int64(ts)))
	n.Add(n, big.NewInt(500_000_000))
	n.Div(n, big.NewInt(1_000_000_000))
	fits := n.IsUint64()
	return n, fits
}

func TestDurationToTicks(t *testing.T) {
	for _, c := range []struct {
		d  int64
		ts uint32
	}{
		{math.MaxInt64, 48000},
		{math.MaxInt64, math.MaxUint32},
		{1, 48000},
		{10417, 48000},
		{10416, 48000},
		{0, 48000},
		{0, math.MaxUint32},
		{math.MaxInt64, 1},
		{1_000_000_000, 44100},
	} {
		want, wantOK := bigTicks(c.d, c.ts)
		got, ok := durationToTicks(time.Duration(c.d), c.ts)
		if ok != wantOK || (ok && got != want.Uint64()) {
			t.Errorf("durationToTicks(%d, %d) = (%d, %v), want (%v, %v)", c.d, c.ts, got, ok, want, wantOK)
		}
	}
	if _, ok := durationToTicks(math.MaxInt64, math.MaxUint32); ok {
		t.Error("durationToTicks(MaxInt64, MaxUint32) = ok, want overflow")
	}
}

func TestRescaleTicks(t *testing.T) {
	if got, ok := rescaleTicks(600, 1000, 48000); !ok || got != 28800 {
		t.Errorf("rescaleTicks(600, 1000, 48000) = (%d, %v), want (28800, true)", got, ok)
	}
	if got, ok := rescaleTicks(1, 3, 1); !ok || got != 0 {
		t.Errorf("rescaleTicks(1, 3, 1) = (%d, %v), want (0, true)", got, ok)
	}
	if got, ok := rescaleTicks(1, 2, 1); !ok || got != 1 {
		t.Errorf("rescaleTicks(1, 2, 1) = (%d, %v), want (1, true) (half rounds up)", got, ok)
	}
	if _, ok := rescaleTicks(math.MaxUint64, 1, math.MaxUint32); ok {
		t.Error("rescaleTicks(MaxUint64, 1, MaxUint32) = ok, want overflow")
	}
	if _, ok := rescaleTicks(5, 0, 48000); ok {
		t.Error("rescaleTicks with from=0 = ok, want false")
	}
}

func TestLocateTick(t *testing.T) {
	const m = math.MaxUint32
	runs := []box.SttsRun{{Count: m, Delta: m}, {Count: m, Delta: m}, {Count: m, Delta: m}}
	span := uint64(m) * uint64(m)
	for _, c := range []struct {
		target uint64
		sample int64
		off    uint64
	}{
		{0, 0, 0},
		{span - 1, m - 1, m - 1},
		{span, m, 0},
		{span + 5, m, 5},
		{math.MaxUint64, int64(m) + 2, 0}, // run 2 holds the last 2^33-2 ticks
	} {
		s, off, ok := locateTick(runs, c.target)
		if !ok || s != c.sample || off != c.off {
			t.Errorf("locateTick(%d) = (%d, %d, %v), want (%d, %d, true)", c.target, s, off, ok, c.sample, c.off)
		}
	}
	// Sample indices above MaxInt32 come back as int64.
	if s, off, ok := locateTick(runs[:1], uint64(m-2)*uint64(m)+5); !ok || s != m-2 || off != 5 {
		t.Errorf("large index = (%d, %d, %v), want (%d, 5, true)", s, off, ok, int64(m-2))
	}
	// Zero-delta runs are skipped but their samples are counted.
	zr := []box.SttsRun{{Count: 3, Delta: 10}, {Count: 4, Delta: 0}, {Count: 2, Delta: 10}}
	if s, off, ok := locateTick(zr, 30); !ok || s != 7 || off != 0 {
		t.Errorf("past zero run = (%d, %d, %v), want (7, 0, true)", s, off, ok)
	}
	if _, _, ok := locateTick(zr, 50); ok {
		t.Error("target equal to the total returned ok")
	}
}

func TestAppendTimeRun(t *testing.T) {
	var runs []box.SttsRun
	runs = appendTimeRun(runs, 0, 5)
	runs = appendTimeRun(runs, 2, 1024)
	runs = appendTimeRun(runs, 3, 1024)
	runs = appendTimeRun(runs, 1, 7)
	if want := []box.SttsRun{{Count: 5, Delta: 1024}, {Count: 1, Delta: 7}}; !slicesEqualRuns(runs, want) {
		t.Fatalf("runs = %+v, want %+v", runs, want)
	}
	// Count overflow starts a new run instead of wrapping.
	o := appendTimeRun(nil, math.MaxUint32, 9)
	o = appendTimeRun(o, 2, 9)
	if want := []box.SttsRun{{Count: math.MaxUint32, Delta: 9}, {Count: 2, Delta: 9}}; !slicesEqualRuns(o, want) {
		t.Fatalf("overflow runs = %+v, want %+v", o, want)
	}
	// In-place compaction over an aliased slice equals a fresh build.
	raw := []box.SttsRun{{Count: 1, Delta: 4}, {Count: 0, Delta: 8}, {Count: 2, Delta: 4}, {Count: 1, Delta: 6}, {Count: 1, Delta: 6}}
	var fresh []box.SttsRun
	for _, r := range raw {
		fresh = appendTimeRun(fresh, r.Count, r.Delta)
	}
	alias := raw[:0]
	for _, r := range raw {
		alias = appendTimeRun(alias, r.Count, r.Delta)
	}
	if !slicesEqualRuns(alias, fresh) || len(fresh) != 2 {
		t.Fatalf("aliased = %+v, fresh = %+v", alias, fresh)
	}
}

func slicesEqualRuns(a, b []box.SttsRun) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSumTicksSaturates(t *testing.T) {
	if got := sumTicks([]box.SttsRun{{Count: 3, Delta: 10}, {Count: 2, Delta: 5}}); got != 40 {
		t.Errorf("sumTicks = %d, want 40", got)
	}
	big3 := []box.SttsRun{{Count: math.MaxUint32, Delta: math.MaxUint32}, {Count: math.MaxUint32, Delta: math.MaxUint32}}
	if got := sumTicks(big3); got != math.MaxUint64 {
		t.Errorf("sumTicks past 2^64 = %d, want saturation at MaxUint64", got)
	}
}

func TestSeekTimeSkipRescalesToSampleRate(t *testing.T) {
	durs := []uint32{1024, 1024}
	data := writeTimed(t, WriterConfig{SampleRate: 16000, Channels: 1, ASC: ascMono16k, EncoderDelay: NoEdit}, durs)
	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	d := tickToDuration(1124, 16000) // 100 ticks into unit 1
	if s, k, err := r.SeekTime(d); err != nil || s != 1 || k != 100 {
		t.Fatalf("equal rate: SeekTime = (%d, %d, %v), want (1, 100, nil)", s, k, err)
	}
	// A media timescale below the decode rate (here 16 kHz ticks, 48 kHz samples)
	// scales the tick offset up to decoded samples; floor division on the way.
	r.info.SampleRate = 48000
	if s, k, err := r.SeekTime(d); err != nil || s != 1 || k != 300 {
		t.Fatalf("rate 48000: SeekTime = (%d, %d, %v), want (1, 300, nil)", s, k, err)
	}
	r.info.SampleRate = 8000
	if s, k, err := r.SeekTime(d); err != nil || s != 1 || k != 50 {
		t.Fatalf("rate 8000: SeekTime = (%d, %d, %v), want (1, 50, nil)", s, k, err)
	}
	// An odd offset (101 ticks) pins rounding down: 101*8000/16000 is 50.5.
	if s, k, err := r.SeekTime(tickToDuration(1125, 16000)); err != nil || s != 1 || k != 50 {
		t.Fatalf("rate 8000, odd offset: SeekTime = (%d, %d, %v), want (1, 50, nil)", s, k, err)
	}
}

func TestErrOutOfRangeMessage(t *testing.T) {
	if got, want := ErrOutOfRange.Error(), "go-m4a: seek position out of range"; got != want {
		t.Fatalf("ErrOutOfRange = %q, want %q", got, want)
	}
}
