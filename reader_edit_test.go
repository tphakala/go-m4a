// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tphakala/go-m4a/internal/box"
)

// setEditList rewrites the version-0 edit list of data (this package's own plain
// or fragmented output) to entries and fixes the edts, trak and moov sizes. Plain
// output has moov after mdat, so the resize leaves the chunk offsets alone; a
// fragmented init segment uses default-base-is-moof, so its offsets are relative.
// It fails the test on a plain file whose moov precedes its mdat.
func setEditList(tb testing.TB, data []byte, entries []box.ElstEntry) []byte {
	tb.Helper()
	moov := topLevelBoxOffset(tb, data, "moov")
	moovSize := int(binary.BigEndian.Uint32(data[moov:]))
	body := data[moov : moov+moovSize]
	if !bytes.Contains(body, []byte("mvex")) && topLevelBoxOffset(tb, data, "mdat") > moov {
		tb.Fatal("setEditList: moov precedes mdat on a plain file, chunk offsets would move")
	}
	edts := bytes.Index(body, []byte("edts")) - 4
	trak := bytes.Index(body, []byte("trak")) - 4
	if edts < 0 || trak < 0 || trak > edts {
		tb.Fatal("setEditList: no trak/edts in moov")
	}
	oldSize := int(binary.BigEndian.Uint32(body[edts:]))

	elstSize := 8 + 8 + 12*len(entries)
	repl := binary.BigEndian.AppendUint32(nil, uint32(8+elstSize))
	repl = append(repl, "edts"...)
	repl = binary.BigEndian.AppendUint32(repl, uint32(elstSize))
	repl = append(repl, "elst"...)
	repl = append(repl, 0, 0, 0, 0)
	repl = binary.BigEndian.AppendUint32(repl, uint32(len(entries)))
	for _, e := range entries {
		repl = binary.BigEndian.AppendUint32(repl, uint32(e.SegmentDuration))
		repl = binary.BigEndian.AppendUint32(repl, uint32(int32(e.MediaTime)))
		repl = binary.BigEndian.AppendUint32(repl, uint32(e.MediaRate))
	}

	out := bytes.Clone(data[:moov+edts])
	out = append(out, repl...)
	out = append(out, data[moov+edts+oldSize:]...)
	delta := len(repl) - oldSize
	for _, off := range []int{moov, moov + trak} {
		binary.BigEndian.PutUint32(out[off:], uint32(int(binary.BigEndian.Uint32(out[off:]))+delta))
	}
	return out
}

// edit builds a rate-1.0 ElstEntry.
func edit(seg uint64, mediaTime int64) box.ElstEntry {
	return box.ElstEntry{SegmentDuration: seg, MediaTime: mediaTime, MediaRate: unitMediaRate}
}

// seekClass names how SeekTime(0) behaves.
type seekClass int

const (
	seekOK seekClass = iota
	seekUnsupported
	seekOutOfRange
)

// windowWant is the expected window in media ticks at 16 kHz (62500 ns a tick).
type windowWant struct {
	delay int64
	dur   uint64
	lead  uint64
	seek  seekClass
}

func ticks16k(n uint64) time.Duration { return time.Duration(n) * 62500 }

func TestPresentationWindow(t *testing.T) {
	const (
		unit   = 1024
		nUnits = 10
		total  = unit * nUnits // 10240 media ticks
		mt     = 1024          // media_time
		seg    = 3000
		lead1  = 1600
		lead2  = 800
	)
	durs := make([]uint32, nUnits)
	for i := range durs {
		durs[i] = unit
	}
	cfg := WriterConfig{SampleRate: 16000, Channels: 1, ASC: ascMono16k}
	plain := writeTimed(t, cfg, durs)
	frag := func() []byte {
		seg := make([]fragAU, nUnits)
		for i := range seg {
			seg[i] = fragAU{au: []byte{byte(i), 0, 1}, dur: unit}
		}
		return buildFragmentedStream(t, cfg, [][]fragAU{seg})
	}()

	dwell := edit(seg, mt)
	dwell.MediaRate = 0

	rows := []struct {
		name    string
		entries []box.ElstEntry
		plain   windowWant
		frag    windowWant
	}{
		{
			"one edit",
			[]box.ElstEntry{edit(seg, mt)},
			windowWant{mt, seg, 0, seekOK},
			windowWant{mt, seg, 0, seekOK},
		},
		{
			"leading empty edit",
			[]box.ElstEntry{edit(lead1, -1), edit(seg, mt)},
			windowWant{mt, seg, lead1, seekOK},
			windowWant{mt, seg, lead1, seekOK},
		},
		{
			"two leading empty edits",
			[]box.ElstEntry{edit(lead1, -1), edit(lead2, -1), edit(seg, mt)},
			windowWant{mt, seg, lead1 + lead2, seekOK},
			windowWant{mt, seg, lead1 + lead2, seekOK},
		},
		// A zero segment is literal on a plain file and open-ended on a fragmented one.
		{
			"zero segment",
			[]box.ElstEntry{edit(0, mt)},
			windowWant{mt, 0, 0, seekOutOfRange},
			windowWant{mt, total - mt, 0, seekOK},
		},
		{
			"segment beyond the media",
			[]box.ElstEntry{edit(total+5000, mt)},
			windowWant{mt, total - mt, 0, seekOK},
			windowWant{mt, total - mt, 0, seekOK},
		},
		{
			"media_time past the media",
			[]box.ElstEntry{edit(seg, total+100)},
			windowWant{total + 100, 0, 0, seekOutOfRange},
			windowWant{total + 100, 0, 0, seekOutOfRange},
		},
		{
			"two non-empty edits",
			[]box.ElstEntry{edit(3000, mt), edit(2000, 5000)},
			windowWant{mt, 5000, 0, seekUnsupported},
			windowWant{mt, 5000, 0, seekUnsupported},
		},
		{
			"trailing empty edit",
			[]box.ElstEntry{edit(seg, mt), edit(lead1, -1)},
			windowWant{mt, seg, 0, seekUnsupported},
			windowWant{mt, seg, 0, seekUnsupported},
		},
		{
			"only an empty edit",
			[]box.ElstEntry{edit(lead1, -1)},
			windowWant{0, 0, lead1, seekUnsupported},
			windowWant{0, 0, lead1, seekUnsupported},
		},
		{
			"dwell",
			[]box.ElstEntry{dwell},
			windowWant{mt, seg, 0, seekUnsupported},
			windowWant{mt, seg, 0, seekUnsupported},
		},
		{
			"media_time -2",
			[]box.ElstEntry{edit(seg, -2)},
			windowWant{0, 0, 0, seekUnsupported},
			windowWant{0, 0, 0, seekUnsupported},
		},
	}
	for _, row := range rows {
		for _, layout := range []struct {
			name string
			data []byte
			want windowWant
		}{{"plain", plain, row.plain}, {"fragmented", frag, row.frag}} {
			t.Run(row.name+"/"+layout.name, func(t *testing.T) {
				r := mustReader(t, setEditList(t, layout.data, row.entries))
				info, want := r.Info(), layout.want
				if info.EncoderDelay != want.delay {
					t.Errorf("EncoderDelay = %d, want %d", info.EncoderDelay, want.delay)
				}
				if wantDur := ticks16k(want.dur); info.Duration != wantDur {
					t.Errorf("Duration = %v, want %v", info.Duration, wantDur)
				}
				if wantOff := ticks16k(want.lead); info.StartOffset != wantOff {
					t.Errorf("StartOffset = %v, want %v", info.StartOffset, wantOff)
				}
				_, _, err := r.SeekTime(0)
				switch want.seek {
				case seekOK:
					if err != nil {
						t.Fatalf("SeekTime(0): %v", err)
					}
					sweepTicks(t, r, durs, 16000, uint64(want.delay), uint64(want.delay)+want.dur)
				case seekUnsupported:
					if !errors.Is(err, ErrUnsupported) {
						t.Errorf("SeekTime(0) err = %v, want ErrUnsupported", err)
					}
				case seekOutOfRange:
					if !errors.Is(err, ErrOutOfRange) {
						t.Errorf("SeekTime(0) err = %v, want ErrOutOfRange", err)
					}
				}
			})
		}
	}
}

// TestOpusShorterThanPreSkip pins the window floor: with no edit list and fewer
// media ticks than the PreSkip, the presented length is 0, not a wrapped value.
func TestOpusShorterThanPreSkip(t *testing.T) {
	cfg := WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 1, OpusPreSkip: 312, EncoderDelay: NoEdit}
	r := mustReader(t, writeTimed(t, cfg, []uint32{120}))
	if info := r.Info(); info.Duration != 0 || info.EncoderDelay != 312 {
		t.Errorf("Duration = %v, EncoderDelay = %d, want 0 and 312", info.Duration, info.EncoderDelay)
	}
	if _, _, err := r.SeekTime(0); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("SeekTime(0) err = %v, want ErrOutOfRange", err)
	}
}

func TestOpusWithoutEditListExcludesPreSkip(t *testing.T) {
	const unit, n, preSkip = 960, 30, 312
	durs := make([]uint32, n)
	for i := range durs {
		durs[i] = unit
	}
	cfg := WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 1, OpusPreSkip: preSkip, EncoderDelay: NoEdit}
	r := mustReader(t, writeTimed(t, cfg, durs))
	presented := uint64(unit*n - preSkip)
	info := r.Info()
	if want := time.Duration(presented * uint64(time.Second) / 48000); info.Duration != want {
		t.Fatalf("Duration = %v, want %v", info.Duration, want)
	}
	if _, _, err := r.SeekTime(tickToDuration(presented-1, 48000)); err != nil {
		t.Errorf("SeekTime(Duration - 1 tick): %v", err)
	}
	if _, _, err := r.SeekTime(info.Duration); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("SeekTime(Duration) err = %v, want ErrOutOfRange", err)
	}
}

// seekResult is one SeekTime outcome.
type seekResult struct {
	sample int
	skip   int64
	oor    bool
}

// TestPresentationWindowAgreesAcrossLayouts pins that the same access units
// report the same EncoderDelay, Duration and SeekTime results however they are
// laid out: plain or fragmented, with or without an edit list (Opus), in one or
// two segments.
func TestPresentationWindowAgreesAcrossLayouts(t *testing.T) {
	build := func(t *testing.T, cfg WriterConfig, n, perSegment int, fragmented bool) *Reader {
		t.Helper()
		if !fragmented {
			durs := make([]uint32, n)
			for i := range durs {
				durs[i] = cfg.unitDuration()
			}
			return mustReader(t, writeTimed(t, cfg, durs))
		}
		var segs [][]fragAU
		for i := 0; i < n; i += perSegment {
			var s []fragAU
			for j := i; j < min(i+perSegment, n); j++ {
				s = append(s, fragAU{au: []byte{byte(j), 0, 1}, dur: cfg.unitDuration()})
			}
			segs = append(segs, s)
		}
		return mustReader(t, buildFragmentedStream(t, cfg, segs))
	}
	opus := WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 1, OpusPreSkip: 312}
	opusNoEdit := opus
	opusNoEdit.EncoderDelay = NoEdit
	aac := WriterConfig{SampleRate: 16000, Channels: 1, ASC: ascMono16k}

	type layout struct {
		name       string
		cfg        WriterConfig
		fragmented bool
		perSegment int
	}
	groups := []struct {
		name    string
		rate    uint32
		layouts []layout
	}{
		{"opus", 48000, []layout{
			{"plain with edit list", opus, false, 0},
			{"plain without edit list", opusNoEdit, false, 0},
			{"fragmented with edit list", opus, true, 100},
			{"fragmented without edit list", opusNoEdit, true, 100},
			{"fragmented in segments", opus, true, 4},
		}},
		{"aac", 16000, []layout{
			{"plain", aac, false, 0},
			{"fragmented", aac, true, 100},
			{"fragmented in segments", aac, true, 3},
		}},
	}
	for _, g := range groups {
		for _, n := range []int{20, 21} {
			var ref *Reader
			var refName string
			for _, l := range g.layouts {
				r := build(t, l.cfg, n, l.perSegment, l.fragmented)
				if ref == nil {
					ref, refName = r, l.name
					continue
				}
				ri, gi := ref.Info(), r.Info()
				if gi.EncoderDelay != ri.EncoderDelay || gi.Duration != ri.Duration || gi.StartOffset != ri.StartOffset {
					t.Errorf("%s n=%d: %s has (delay %d, dur %v, off %v), %s has (delay %d, dur %v, off %v)",
						g.name, n, l.name, gi.EncoderDelay, gi.Duration, gi.StartOffset,
						refName, ri.EncoderDelay, ri.Duration, ri.StartOffset)
				}
				end := uint64(ri.EncoderDelay) + uint64(ri.Duration)*uint64(g.rate)/uint64(time.Second)
				for tick := uint64(0); tick < end+3; tick++ {
					d := tickToDuration(tick, g.rate)
					var want, got seekResult
					s, k, err := ref.SeekTime(d)
					want = seekResult{s, k, errors.Is(err, ErrOutOfRange)}
					s, k, err = r.SeekTime(d)
					got = seekResult{s, k, errors.Is(err, ErrOutOfRange)}
					if got != want {
						t.Fatalf("%s n=%d: SeekTime(tick %d) %s = %+v, %s = %+v", g.name, n, tick, l.name, got, refName, want)
					}
				}
			}
		}
	}
}

// unitDuration is the access-unit duration the layout tests use for cfg: 960
// ticks for Opus (20 ms at 48 kHz), 1024 otherwise.
func (c WriterConfig) unitDuration() uint32 {
	if c.Codec == CodecOpus {
		return 960
	}
	return 1024
}

// TestResolvePresentationSaturates pins that sums which overflow uint64 saturate
// instead of wrapping into a small window.
func TestResolvePresentationSaturates(t *testing.T) {
	huge := []box.SttsRun{{Count: math.MaxUint32, Delta: math.MaxUint32}, {Count: math.MaxUint32, Delta: math.MaxUint32}}
	tr := &track{mdhdTS: 1, edits: []box.ElstEntry{
		edit(math.MaxUint64, -1), edit(math.MaxUint64, -1), edit(0, 1), edit(0, 1),
	}}
	w := resolvePresentation(tr, 1, huge, true)
	if w.lead != math.MaxUint64 || w.presented != math.MaxUint64 || w.end != math.MaxUint64 {
		t.Errorf("lead, presented, end = %d, %d, %d, want all MaxUint64", w.lead, w.presented, w.end)
	}
	if !errors.Is(w.seekErr, ErrUnsupported) {
		t.Errorf("seekErr = %v, want ErrUnsupported", w.seekErr)
	}
}

func TestTicksToDuration(t *testing.T) {
	tests := []struct {
		name  string
		ticks uint64
		ts    uint32
		want  time.Duration
	}{
		{"floor", 1, 3, 333333333},
		{"exact", 48000, 48000, time.Second},
		{"zero timescale", 5, 0, 0},
		{"zero ticks", 0, 48000, 0},
		{"saturates when the quotient does not fit", 2e10, 1, math.MaxInt64},
		{"saturates above MaxInt64", math.MaxUint64, 1e9, math.MaxInt64},
		{"largest exact", 9_000_000_000, 1, 9_000_000_000 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ticksToDuration(tc.ticks, tc.ts); got != tc.want {
				t.Errorf("ticksToDuration(%d, %d) = %d, want %d", tc.ticks, tc.ts, got, tc.want)
			}
		})
	}
}

// TestFFprobeLeadingEmptyEdit compares StartOffset with the start_time ffprobe
// reports for a file whose edit list opens with an empty edit.
func TestFFprobeLeadingEmptyEdit(t *testing.T) {
	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
	durs := make([]uint32, 40)
	for i := range durs {
		durs[i] = 1024
	}
	data := setEditList(t, writeTimed(t, cfg, durs), []box.ElstEntry{edit(24000, -1), edit(30000, 1024)})
	path := filepath.Join(t.TempDir(), "lead.m4a")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	r := mustReader(t, data)
	assertWithinMicrosecond(t, "StartOffset", r.Info().StartOffset, ffprobeStreamField(t, path, "start_time"))
	if r.Info().StartOffset != 500*time.Millisecond {
		t.Errorf("StartOffset = %v, want 500ms", r.Info().StartOffset)
	}
}

// TestStartOffsetUsesMovieTimescale pins that leading empty edits are converted
// with the movie timescale, not the media timescale: an empty edit of 500 movie
// ticks at 1000 ticks per second is 500 ms whatever the media rate.
func TestStartOffsetUsesMovieTimescale(t *testing.T) {
	tr := &track{mdhdTS: 48000, edits: []box.ElstEntry{edit(500, -1), edit(1000, 1024)}}
	var rd Reader
	rd.setPresentation(tr, 1000, []box.SttsRun{{Count: 10, Delta: 1024}}, false)
	if got := rd.Info().StartOffset; got != 500*time.Millisecond {
		t.Errorf("StartOffset = %v, want 500ms", got)
	}
	// The 1000-tick segment is one second of movie time: 48000 media ticks, clipped
	// to the 10240 ticks that follow media_time 1024.
	if want := ticksToDuration(10240-1024, 48000); rd.Info().Duration != want {
		t.Errorf("Duration = %v, want %v", rd.Info().Duration, want)
	}
}

// TestTruncatedEditListIsCorrupt pins that an elst whose entry_count overruns its
// body fails NewReader with ErrCorrupt, on the plain and the fragmented path.
func TestTruncatedEditListIsCorrupt(t *testing.T) {
	cfg := WriterConfig{SampleRate: 16000, Channels: 1, ASC: ascMono16k}
	plain := writeTimed(t, cfg, []uint32{1024, 1024})
	frag := buildFragmentedStream(t, cfg, [][]fragAU{{{au: []byte{1, 0, 1}, dur: 1024}}})
	for name, data := range map[string][]byte{"plain": plain, "fragmented": frag} {
		t.Run(name, func(t *testing.T) {
			data = setEditList(t, data, []box.ElstEntry{edit(1000, 1024)})
			moov := topLevelBoxOffset(t, data, "moov")
			elst := moov + bytes.Index(data[moov:], []byte("elst"))
			binary.BigEndian.PutUint32(data[elst+8:], 5) // entry_count after version/flags
			if _, err := NewReader(bytes.NewReader(data)); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("NewReader err = %v, want ErrCorrupt", err)
			}
		})
	}
}

// TestStartOffsetWiring pins that NewReader passes the movie timescale from the
// mvhd, not the media timescale, to the edit-list resolver, on the plain and the
// fragmented path. The movie header is patched to 1000 ticks per second while the
// media stays at 16000.
func TestStartOffsetWiring(t *testing.T) {
	cfg := WriterConfig{SampleRate: 16000, Channels: 1, ASC: ascMono16k}
	durs := []uint32{1024, 1024, 1024, 1024}
	au := make([]fragAU, 0, len(durs))
	for i, d := range durs {
		au = append(au, fragAU{au: []byte{byte(i), 0, 1}, dur: d})
	}
	files := map[string][]byte{
		"plain":      writeTimed(t, cfg, durs),
		"fragmented": buildFragmentedStream(t, cfg, [][]fragAU{au}),
	}
	for name, data := range files {
		t.Run(name, func(t *testing.T) {
			data = setEditList(t, data, []box.ElstEntry{edit(500, -1), edit(100, 1024)})
			moov := topLevelBoxOffset(t, data, "moov")
			mvhd := moov + bytes.Index(data[moov:], []byte("mvhd"))
			// mvhd version 0: type(4) version/flags(4) creation(4) modification(4) timescale(4).
			binary.BigEndian.PutUint32(data[mvhd+16:], 1000)
			info := mustReader(t, data).Info()
			if info.StartOffset != 500*time.Millisecond {
				t.Errorf("StartOffset = %v, want 500ms", info.StartOffset)
			}
			// 100 movie ticks are 100 ms: 1600 media ticks.
			if want := ticks16k(1600); info.Duration != want {
				t.Errorf("Duration = %v, want %v", info.Duration, want)
			}
		})
	}
}

// TestSeekTimeHalfTickBeforeEnd pins the documented boundary: a time less than
// half a media tick past the last tick still selects it, and one more than half a
// tick past it rounds to the end and is refused.
func TestSeekTimeHalfTickBeforeEnd(t *testing.T) {
	cfg := WriterConfig{SampleRate: 16000, Channels: 1, ASC: ascMono16k, EncoderDelay: NoEdit}
	r := mustReader(t, writeTimed(t, cfg, []uint32{1024, 1024, 1024, 1024}))
	last := ticks16k(4095)
	if s, _, err := r.SeekTime(last + ticks16k(1)*4/10); err != nil || s != 3 {
		t.Errorf("SeekTime(last tick + 0.4 tick) = (%d, %v), want unit 3", s, err)
	}
	if _, _, err := r.SeekTime(last + ticks16k(1)*6/10); !errors.Is(err, ErrOutOfRange) {
		t.Errorf("SeekTime(last tick + 0.6 tick) err = %v, want ErrOutOfRange", err)
	}
}
