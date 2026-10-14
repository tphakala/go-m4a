// SPDX-License-Identifier: MIT

package aacm4a

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	aac "github.com/tphakala/go-aac"
	aacpcm "github.com/tphakala/go-aac/pcm"

	"github.com/tphakala/go-m4a/internal/testutil"

	m4a "github.com/tphakala/go-m4a"
)

// frameBytes is the size of one decoded access unit in bytes.
func frameBytes(f *fixture) int { return aac.FrameSize * f.stride() }

// requireClose checks that got and want are the same length and that every
// access unit's energy is within maxDB of the reference and every channel
// correlates at least minCorr over the whole compared span.
func requireClose(t *testing.T, what string, got, want []byte, channels int, maxDB, minCorr float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: length %d, want %d", what, len(got), len(want))
	}
	au := aac.FrameSize * channels * 2
	worst := 0.0
	for off := 0; off+au <= len(want); off += au {
		var eg, ew float64
		for c := range channels {
			for _, v := range testutil.ChannelS16(got[off:off+au], channels, c) {
				eg += v * v
			}
			for _, v := range testutil.ChannelS16(want[off:off+au], channels, c) {
				ew += v * v
			}
		}
		if ew < 1 && eg < 1 {
			continue
		}
		db := math.Abs(10 * math.Log10((eg+1)/(ew+1)))
		worst = max(worst, db)
		if db > maxDB {
			t.Fatalf("%s: access unit at byte %d differs by %.3f dB, limit %.3f", what, off, db, maxDB)
		}
	}
	minSeen := 1.0
	for c := range channels {
		corr := testutil.NormXCorr(testutil.ChannelS16(want, channels, c), testutil.ChannelS16(got, channels, c), 0)
		minSeen = min(minSeen, corr)
		if corr < minCorr {
			t.Fatalf("%s: channel %d correlation %.4f, limit %.4f", what, c, corr, minCorr)
		}
	}
	t.Logf("%s: worst energy difference %.4f dB, min correlation %.4f", what, worst, minSeen)
}

func TestSeekableDecoderMatchesNewDecoder(t *testing.T) {
	fixtures := make(map[string]fixture)
	for _, tc := range exactConfigs {
		fixtures[tc.name] = exactFixture(t, tc.rate, tc.channels)
	}
	fixtures["pns48kStereo"] = pnsFixture(t, 48000, 2, 64000)
	for name, f := range fixtures {
		t.Run(name, func(t *testing.T) {
			d, _ := newSeekable(t, f.data)
			got, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			requireSame(t, "Read", got, linear(t, f.data))

			d2, _ := newSeekable(t, f.data)
			var buf bytes.Buffer
			if _, err := io.Copy(&buf, d2); err != nil {
				t.Fatalf("Copy: %v", err)
			}
			requireSame(t, "WriteTo", buf.Bytes(), got)
		})
	}
}

func TestSeekSampleBitExact(t *testing.T) {
	for _, tc := range exactConfigs {
		t.Run(tc.name, func(t *testing.T) {
			f := exactFixture(t, tc.rate, tc.channels)
			want := linear(t, f.data)
			d, info := newSeekable(t, f.data)
			fc := info.FrameCount
			for _, target := range []int{0, 1, seekPreRoll, seekPreRoll + 1, fc / 3, fc / 2, fc - 2, fc - 1} {
				if err := d.SeekSample(target); err != nil {
					t.Fatalf("SeekSample(%d): %v", target, err)
				}
				got, err := io.ReadAll(d)
				if err != nil {
					t.Fatalf("ReadAll after SeekSample(%d): %v", target, err)
				}
				requireSame(t, "remainder after SeekSample", got, want[target*frameBytes(&f):])
			}
		})
	}
}

func TestSeekTimeBitExact(t *testing.T) {
	for _, tc := range exactConfigs {
		t.Run(tc.name, func(t *testing.T) {
			f := exactFixture(t, tc.rate, tc.channels)
			want := linear(t, f.data)
			d, info := newSeekable(t, f.data)
			presented := presentedSamples(info)
			for _, n := range []int64{0, 1, 1023, 1024, 1025, presented / 2, presented - 1} {
				if err := d.SeekTime(sampleTime(n, tc.rate)); err != nil {
					t.Fatalf("SeekTime(%d samples): %v", n, err)
				}
				got, err := io.ReadAll(d)
				if err != nil {
					t.Fatalf("ReadAll after SeekTime(%d samples): %v", n, err)
				}
				requireSame(t, "remainder after SeekTime", got, want[(info.EncoderDelay+n)*int64(f.stride()):])
			}
		})
	}
}

// TestSeekWithoutPreRollDiverges proves the exactness tests can fail: with no
// pre-roll the output differs from the linear decode, with the pre-roll it does
// not.
func TestSeekWithoutPreRollDiverges(t *testing.T) {
	f := exactFixture(t, 44100, 2)
	want := linear(t, f.data)
	d, info := newSeekable(t, f.data)
	target := info.FrameCount / 2
	for _, preRoll := range []int{0, seekPreRoll} {
		if err := d.restart(target, 0, preRoll); err != nil {
			t.Fatalf("restart: %v", err)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		same := bytes.Equal(got, want[target*frameBytes(&f):])
		if preRoll == 0 && same {
			t.Errorf("pre-roll 0 matched the linear decode, so the exactness tests cannot fail")
		}
		if preRoll == seekPreRoll && !same {
			t.Errorf("pre-roll %d did not match the linear decode", preRoll)
		}
	}
}

// TestSeekableDecoderSeekSequence replays random operations against a model that
// is just a byte position in the linear decode.
func TestSeekableDecoderSeekSequence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		rate, ch int
	}{{"mono44k", 44100, 1}, {"stereo48k", 48000, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			f := exactFixture(t, tc.rate, tc.ch)
			want := linear(t, f.data)
			stride := int64(f.stride())
			for seed := uint64(1); seed <= 8; seed++ {
				rng := rand.New(rand.NewPCG(seed, seed*977)) //nolint:gosec // G404: a fixed-seed operation script, not a security boundary.
				d, info := newSeekable(t, f.data)
				presented := presentedSamples(info)
				fc := int64(info.FrameCount)
				pos := int64(0)
				for op := range 200 {
					switch rng.IntN(5) {
					case 0:
						k := rng.Int64N(fc+5) - 2
						err := d.SeekSample(int(k))
						if k >= 0 && k < fc {
							if err != nil {
								t.Fatalf("seed %d op %d: SeekSample(%d): %v", seed, op, k, err)
							}
							pos = k * aac.FrameSize * stride
						} else if !errors.Is(err, m4a.ErrOutOfRange) {
							t.Fatalf("seed %d op %d: SeekSample(%d) = %v, want ErrOutOfRange", seed, op, k, err)
						}
					case 1:
						n := rng.Int64N(presented+5) - 2
						err := d.SeekTime(sampleTime(n, tc.rate))
						if n >= 0 && n < presented {
							if err != nil {
								t.Fatalf("seed %d op %d: SeekTime(%d samples): %v", seed, op, n, err)
							}
							pos = (info.EncoderDelay + n) * stride
						} else if !errors.Is(err, m4a.ErrOutOfRange) {
							t.Fatalf("seed %d op %d: SeekTime(%d samples) = %v, want ErrOutOfRange", seed, op, n, err)
						}
					case 2:
						if err := d.SeekTime(info.Duration); !errors.Is(err, m4a.ErrOutOfRange) {
							t.Fatalf("seed %d op %d: SeekTime(Duration) = %v, want ErrOutOfRange", seed, op, err)
						}
					case 3:
						if rng.IntN(8) != 0 {
							continue
						}
						var buf bytes.Buffer
						if _, err := d.WriteTo(&buf); err != nil {
							t.Fatalf("seed %d op %d: WriteTo: %v", seed, op, err)
						}
						requireSame(t, "WriteTo", buf.Bytes(), want[pos:])
						pos = int64(len(want))
						if n, err := d.Read(make([]byte, 16)); n != 0 || !errors.Is(err, io.EOF) {
							t.Fatalf("seed %d op %d: Read after WriteTo = %d, %v, want 0, EOF", seed, op, n, err)
						}
					default:
						buf := make([]byte, 1+rng.IntN(9000))
						n, err := d.Read(buf)
						if pos == int64(len(want)) {
							if n != 0 || !errors.Is(err, io.EOF) {
								t.Fatalf("seed %d op %d: Read at end = %d, %v, want 0, EOF", seed, op, n, err)
							}
							continue
						}
						if err != nil || n == 0 {
							t.Fatalf("seed %d op %d: Read = %d, %v at %d of %d", seed, op, n, err, pos, len(want))
						}
						requireSame(t, "Read", buf[:n], want[pos:pos+int64(n)])
						pos += int64(n)
					}
				}
			}
		})
	}
}

func TestSeekOutOfRangeKeepsPosition(t *testing.T) {
	f := exactFixture(t, 48000, 1)
	want := linear(t, f.data)
	d, info := newSeekable(t, f.data)
	head := make([]byte, 0, len(want))
	head = head[:3001]
	if _, err := io.ReadFull(d, head); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	for name, err := range map[string]error{
		"SeekSample(-1)":       d.SeekSample(-1),
		"SeekSample(FC)":       d.SeekSample(info.FrameCount),
		"SeekTime(-1ns)":       d.SeekTime(-time.Nanosecond),
		"SeekTime(Duration)":   d.SeekTime(info.Duration),
		"SeekTime(Duration+1)": d.SeekTime(info.Duration + time.Hour),
	} {
		if !errors.Is(err, m4a.ErrOutOfRange) {
			t.Errorf("%s = %v, want ErrOutOfRange", name, err)
		}
	}
	rest, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	requireSame(t, "head plus rest", append(head, rest...), want)
}

func TestSeekTimeUnsupportedEditList(t *testing.T) {
	f := exactFixture(t, 48000, 1)
	want := linear(t, f.data)
	data := bytes.Clone(f.data)
	i := bytes.Index(data, []byte("elst"))
	if i < 0 {
		t.Fatal("no elst box in the fixture")
	}
	// Past the fourcc: version and flags, entry count, then the first entry's
	// segment duration and media time (4 bytes each in version 0, 8 in version 1),
	// then media_rate_integer.
	rateOff := i + 4 + 4 + 4 + 8
	if data[i+4] == 1 {
		rateOff = i + 4 + 4 + 4 + 16
	}
	binary.BigEndian.PutUint16(data[rateOff:], 2)

	d, _ := newSeekable(t, data)
	head := make([]byte, 0, len(want))
	head = head[:5000]
	if _, err := io.ReadFull(d, head); err != nil {
		t.Fatalf("ReadFull: %v", err)
	}
	if err := d.SeekTime(0); !errors.Is(err, m4a.ErrUnsupported) {
		t.Fatalf("SeekTime(0) = %v, want ErrUnsupported", err)
	}
	rest, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	requireSame(t, "head plus rest", append(head, rest...), want)

	if err := d.SeekSample(40); err != nil {
		t.Fatalf("SeekSample after the rejected SeekTime: %v", err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	requireSame(t, "remainder after SeekSample", got, want[40*frameBytes(&f):])
}

// auRanges returns each access unit's byte range in data, which must be a plain
// file from m4a.Writer with the units contiguous in mdat.
func auRanges(t *testing.T, data []byte) [][2]int {
	t.Helper()
	rd, err := m4a.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	start, _ := testutil.MDATPayload(t, data)
	var out [][2]int
	off := start
	for {
		au, err := rd.ReadFrame()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if !bytes.Equal(data[off:off+len(au)], au) {
			t.Fatalf("access unit %d is not contiguous in mdat", len(out))
		}
		out = append(out, [2]int{off, off + len(au)})
		off += len(au)
	}
}

func TestSeekableDecoderErrorLatchAndRecovery(t *testing.T) {
	f := exactFixture(t, 44100, 1)
	clean := linear(t, f.data)
	ranges := auRanges(t, f.data)
	k := len(ranges) / 2
	bad := bytes.Clone(f.data)
	for i := ranges[k][0]; i < ranges[k][1]; i++ {
		bad[i] = 0xFF // first element id is ID_END: an access unit with no audio
	}

	d, _ := newSeekable(t, bad)
	got, err := io.ReadAll(d)
	if !errors.Is(err, aacpcm.ErrCorruptStream) || !errors.Is(err, m4a.ErrCorrupt) {
		t.Fatalf("ReadAll error = %v, want both aacpcm.ErrCorruptStream and m4a.ErrCorrupt", err)
	}
	requireSame(t, "output before the bad unit", got, clean[:k*frameBytes(&f)])
	if n, err2 := d.Read(make([]byte, 8)); n != 0 || !errors.Is(err2, err) {
		t.Fatalf("second Read = %d, %v, want 0 and the latched %v", n, err2, err)
	}
	if _, err2 := d.WriteTo(io.Discard); !errors.Is(err2, err) {
		t.Fatalf("WriteTo = %v, want the latched %v", err2, err)
	}

	if err := d.SeekSample(k + 1 + seekPreRoll); err != nil {
		t.Fatalf("SeekSample past the bad unit: %v", err)
	}
	rest, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll after recovery: %v", err)
	}
	requireSame(t, "remainder after recovery", rest, clean[(k+1+seekPreRoll)*frameBytes(&f):])

	// The pre-roll of a seek just after the bad unit decodes it, and that error
	// must surface rather than be skipped.
	if err := d.SeekSample(k + 1); err != nil {
		t.Fatalf("SeekSample(k+1) is lazy and must succeed: %v", err)
	}
	if n, err := d.Read(make([]byte, 64)); n != 0 || !errors.Is(err, m4a.ErrCorrupt) {
		t.Fatalf("Read through a bad pre-roll unit = %d, %v, want 0 and ErrCorrupt", n, err)
	}

	// The documented divergence from NewDecoder: its decoder drops the unit.
	nd, _, err := NewDecoder(bytes.NewReader(bad))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if _, err := io.ReadAll(nd); err != nil {
		t.Fatalf("NewDecoder on a unit with no audio element now errors (%v); update the Decoder docs", err)
	}
}

func TestSeekableDecoderFragmented(t *testing.T) {
	f := exactFixture(t, 48000, 2)
	stream, segments := fragmentedStream(t, f.aus, f.wcfg)
	if segments < 2 {
		t.Fatalf("got %d segments, want at least 2", segments)
	}
	want := linear(t, stream)
	d, info := newSeekable(t, stream)
	fc := info.FrameCount
	for _, target := range []int{0, 1, seekPreRoll + 1, fc / 3, fc / 2, fc - 1} {
		if err := d.SeekSample(target); err != nil {
			t.Fatalf("SeekSample(%d): %v", target, err)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		requireSame(t, "remainder after SeekSample", got, want[target*frameBytes(&f):])
	}
	presented := presentedSamples(info)
	for _, n := range []int64{0, 1, 1025, presented / 2, presented - 1} {
		if err := d.SeekTime(sampleTime(n, 48000)); err != nil {
			t.Fatalf("SeekTime(%d samples): %v", n, err)
		}
		got, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
		requireSame(t, "remainder after SeekTime", got, want[(info.EncoderDelay+n)*int64(f.stride()):])
	}
}

func TestSeekTimeIntoOverlongUnit(t *testing.T) {
	f := exactFixture(t, 48000, 1)
	const j = 10
	var ws testutil.WriteSeeker
	w, err := m4a.NewWriter(&ws, f.wcfg)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, au := range f.aus {
		dur := uint32(aac.FrameSize)
		if i == j {
			dur = 4096
		}
		if err := w.WriteFrameDuration(au, dur); err != nil {
			t.Fatalf("WriteFrameDuration: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data := ws.Bytes()
	want := linear(t, data)
	d, info := newSeekable(t, data)

	// Presentation time n is media tick EncoderDelay+n; unit j starts at tick
	// j*1024 and spans 4096 ticks.
	at := func(tick int64) time.Duration { return sampleTime(tick-info.EncoderDelay, 48000) }

	if err := d.SeekTime(at(j*aac.FrameSize + 500)); err != nil {
		t.Fatalf("SeekTime into the first 1024 samples of the long unit: %v", err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	requireSame(t, "remainder", got, want[(j*aac.FrameSize+500)*f.stride():])

	err = d.SeekTime(at(j*aac.FrameSize + 2000))
	if !errors.Is(err, m4a.ErrUnsupported) {
		t.Fatalf("SeekTime into the long unit's second half = %v, want ErrUnsupported", err)
	}
	if n, err2 := d.Read(make([]byte, 64)); n != 0 || !errors.Is(err2, m4a.ErrUnsupported) {
		t.Fatalf("Read after the refused seek = %d, %v, want 0 and ErrUnsupported", n, err2)
	}
	if err := d.SeekSample(20); err != nil {
		t.Fatalf("SeekSample: %v", err)
	}
	got, err = io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	requireSame(t, "remainder after recovery", got, want[20*frameBytes(&f):])
}

func TestSeekableDecoderPNS(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rate, ch, bps int
	}{{"stereo48k64k", 48000, 2, 64000}, {"mono44k32k", 44100, 1, 32000}} {
		t.Run(tc.name, func(t *testing.T) {
			f := pnsFixture(t, tc.rate, tc.ch, tc.bps)
			want := linear(t, f.data)
			d1, info := newSeekable(t, f.data)
			d2, _ := newSeekable(t, f.data)
			fc := info.FrameCount
			for _, target := range []int{0, 1, seekPreRoll, fc / 3, fc / 2} {
				if err := d1.SeekSample(target); err != nil {
					t.Fatalf("SeekSample: %v", err)
				}
				if err := d2.SeekSample(target); err != nil {
					t.Fatalf("SeekSample: %v", err)
				}
				a, err := io.ReadAll(d1)
				if err != nil {
					t.Fatalf("ReadAll: %v", err)
				}
				b, err := io.ReadAll(d2)
				if err != nil {
					t.Fatalf("ReadAll: %v", err)
				}
				requireSame(t, "two decoders at the same target", a, b)
				ref := want[target*frameBytes(&f):]
				if target <= seekPreRoll {
					requireSame(t, "seek near the start", a, ref)
					continue
				}
				requireClose(t, "remainder", a, ref, tc.ch, 0.5, 0.95)
			}
		})
	}
}

func TestNewSeekableDecoderHEAACSentinels(t *testing.T) {
	base := buildAACContainer(t, ascAACLC)
	for _, tc := range []struct {
		name   string
		asc    []byte
		wantPS bool
	}{{"HE-AAC", ascHEAAC, false}, {"HE-AACv2", ascHEAACv2, true}} {
		t.Run(tc.name, func(t *testing.T) {
			data := rewriteASC(t, base, ascAACLC, tc.asc)
			d, info, err := NewSeekableDecoder(bytes.NewReader(data))
			if err == nil || d != nil {
				t.Fatalf("NewSeekableDecoder = %v, %v, want a nil decoder and an error", d, err)
			}
			if !errors.Is(err, aacpcm.ErrUnsupportedSBR) || !errors.Is(err, aacpcm.ErrUnsupported) || !errors.Is(err, m4a.ErrUnsupported) {
				t.Errorf("err = %v, want aacpcm.ErrUnsupportedSBR, aacpcm.ErrUnsupported and m4a.ErrUnsupported", err)
			}
			if got := errors.Is(err, aacpcm.ErrUnsupportedPS); got != tc.wantPS {
				t.Errorf("errors.Is(err, ErrUnsupportedPS) = %v, want %v", got, tc.wantPS)
			}
			if info.Codec != m4a.CodecAACLC {
				t.Errorf("info.Codec = %v, want the Info returned alongside the error", info.Codec)
			}
		})
	}

	t.Run("reserved sampling index", func(t *testing.T) {
		// Sampling frequency index 13 is reserved: object type 2, index 13, one channel.
		data := rewriteASC(t, base, ascAACLC, []byte{0x16, 0x88, 0x00})
		d, _, err := NewSeekableDecoder(bytes.NewReader(data))
		if err == nil || d != nil {
			t.Fatalf("NewSeekableDecoder = %v, %v, want a nil decoder and an error", d, err)
		}
		t.Logf("reserved sampling index: %v", err)
		if !errors.Is(err, m4a.ErrCorrupt) && !errors.Is(err, m4a.ErrUnsupported) {
			t.Errorf("err = %v, want an m4a sentinel", err)
		}
	})
}

func TestNewSeekableDecoderNotAAC(t *testing.T) {
	for _, name := range []string{"opus_mono48k.mp4", "flac_mono44k.mp4"} {
		data, err := os.ReadFile(filepath.Join("..", "testdata", "interop", name))
		if err != nil {
			t.Fatal(err)
		}
		d, _, err := NewSeekableDecoder(bytes.NewReader(data))
		if d != nil || !errors.Is(err, m4a.ErrUnsupported) {
			t.Errorf("%s: NewSeekableDecoder = %v, %v, want a nil decoder and ErrUnsupported", name, d, err)
		}
	}
}

func TestNewSeekableDecoderGarbage(t *testing.T) {
	d, _, err := NewSeekableDecoder(bytes.NewReader([]byte("this is definitely not an mp4 container")))
	if err == nil || d != nil {
		t.Fatalf("NewSeekableDecoder = %v, %v, want a nil decoder and an error", d, err)
	}
}

func TestDecodersForwardReaderOptions(t *testing.T) {
	f := exactFixture(t, 48000, 1)
	if _, _, err := NewDecoder(bytes.NewReader(f.data)); err != nil {
		t.Fatalf("NewDecoder without options: %v", err)
	}
	if _, _, err := NewSeekableDecoder(bytes.NewReader(f.data)); err != nil {
		t.Fatalf("NewSeekableDecoder without options: %v", err)
	}
	if _, _, err := NewDecoder(bytes.NewReader(f.data), m4a.WithMaxBoxBuffer(16)); !errors.Is(err, m4a.ErrBoxTooLarge) {
		t.Errorf("NewDecoder with a 16-byte box limit = %v, want ErrBoxTooLarge", err)
	}
	if _, _, err := NewSeekableDecoder(bytes.NewReader(f.data), m4a.WithMaxBoxBuffer(16)); !errors.Is(err, m4a.ErrBoxTooLarge) {
		t.Errorf("NewSeekableDecoder with a 16-byte box limit = %v, want ErrBoxTooLarge", err)
	}
}

func TestSeekableDecoderOversizedAccessUnit(t *testing.T) {
	asc, err := audioSpecificConfig(48000, 1)
	if err != nil {
		t.Fatal(err)
	}
	var ws testutil.WriteSeeker
	w, err := m4a.NewWriter(&ws, m4a.WriterConfig{SampleRate: 48000, Channels: 1, ASC: asc})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteFrame(make([]byte, 70000)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	d, _ := newSeekable(t, ws.Bytes())
	if n, err := d.Read(make([]byte, 64)); n != 0 || !errors.Is(err, m4a.ErrUnsupported) {
		t.Fatalf("Read = %d, %v, want 0 and ErrUnsupported", n, err)
	}
	if cap(d.au) > maxAccessUnit {
		t.Errorf("access unit buffer grew to %d, above the %d ceiling", cap(d.au), maxAccessUnit)
	}
	nd, _, err := NewDecoder(bytes.NewReader(ws.Bytes()))
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	if _, err := io.ReadAll(nd); !errors.Is(err, m4a.ErrUnsupported) {
		t.Errorf("NewDecoder decode of the same file = %v, want ErrUnsupported", err)
	}
}

func TestSeekableDecoderReadAllocs(t *testing.T) {
	f := exactFixture(t, 48000, 2)
	d, _ := newSeekable(t, f.data)
	buf := make([]byte, frameBytes(&f))
	if _, err := io.ReadFull(d, buf); err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(20, func() {
		if _, err := io.ReadFull(d, buf); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Errorf("steady-state Read allocates %v times per access unit, want 0", n)
	}
	seekRead := testing.AllocsPerRun(20, func() {
		if err := d.SeekSample(50); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(d, buf); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("seek plus first read: %v allocations", seekRead)
	if seekRead != 0 {
		t.Errorf("seek plus first read allocates %v times, want 0", seekRead)
	}
}

func TestSeekableDecoderInteropAfconvert(t *testing.T) {
	for _, name := range []string{"afconvert_mono48k.m4a", "afconvert_stereo44k.m4a"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "testdata", "interop", name))
			if err != nil {
				t.Fatal(err)
			}
			want := linear(t, data)
			d, info := newSeekable(t, data)
			stride := info.Channels * 2
			for _, target := range []int{1, 2, info.FrameCount / 2, info.FrameCount - 1} {
				if err := d.SeekSample(target); err != nil {
					t.Fatalf("SeekSample(%d): %v", target, err)
				}
				got, err := io.ReadAll(d)
				if err != nil {
					t.Fatalf("ReadAll: %v", err)
				}
				requireSame(t, "remainder", got, want[target*aac.FrameSize*stride:])
			}
		})
	}
}

// TestSeekableDecoderInteropFFmpeg checks position and length on ffmpeg-encoded
// files, which use PNS, so the samples are close rather than identical.
func TestSeekableDecoderInteropFFmpeg(t *testing.T) {
	for _, name := range []string{"ffmpeg_mono48k.m4a", "ffmpeg_stereo44k.m4a", "ffmpeg_noedit_mono48k.m4a"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "testdata", "interop", name))
			if err != nil {
				t.Fatal(err)
			}
			want := linear(t, data)
			d, info := newSeekable(t, data)
			stride := info.Channels * 2
			for _, target := range []int{1, 2, info.FrameCount / 2, info.FrameCount - 1} {
				if err := d.SeekSample(target); err != nil {
					t.Fatalf("SeekSample(%d): %v", target, err)
				}
				got, err := io.ReadAll(d)
				if err != nil {
					t.Fatalf("ReadAll: %v", err)
				}
				requireClose(t, "remainder", got, want[target*aac.FrameSize*stride:], info.Channels, ffmpegMaxDB, ffmpegMinCorr)
			}
		})
	}
}

// Bounds for the ffmpeg fixtures. Measured: at most 0.0002 dB per access unit
// and a correlation that rounds to 1.0000; these leave wide margin.
const (
	ffmpegMaxDB   = 0.1
	ffmpegMinCorr = 0.99
)

// failingWriter accepts limit bytes, then fails (err set) or short-writes (err
// nil, the writer reports fewer bytes than given without an error).
type failingWriter struct {
	buf   bytes.Buffer
	limit int
	err   error
}

func (w *failingWriter) Write(p []byte) (int, error) {
	n := min(len(p), max(0, w.limit-w.buf.Len()))
	w.buf.Write(p[:n])
	if n < len(p) {
		return n, w.err
	}
	return n, nil
}

func TestSeekableDecoderWriteToFailures(t *testing.T) {
	f := exactFixture(t, 44100, 1)
	clean := linear(t, f.data)
	ranges := auRanges(t, f.data)
	k := len(ranges) / 2

	t.Run("decode error", func(t *testing.T) {
		bad := bytes.Clone(f.data)
		for i := ranges[k][0]; i < ranges[k][1]; i++ {
			bad[i] = 0xFF
		}
		d, _ := newSeekable(t, bad)
		var buf bytes.Buffer
		n, err := d.WriteTo(&buf)
		if !errors.Is(err, aacpcm.ErrCorruptStream) || !errors.Is(err, m4a.ErrCorrupt) {
			t.Fatalf("WriteTo error = %v, want both aacpcm.ErrCorruptStream and m4a.ErrCorrupt", err)
		}
		if n != int64(k*frameBytes(&f)) {
			t.Errorf("WriteTo count = %d, want %d", n, k*frameBytes(&f))
		}
		requireSame(t, "bytes written before the error", buf.Bytes(), clean[:k*frameBytes(&f)])
	})

	t.Run("writer error", func(t *testing.T) {
		d, _ := newSeekable(t, f.data)
		boom := errors.New("disk full")
		w := &failingWriter{limit: 1000, err: boom}
		n, err := d.WriteTo(w)
		if !errors.Is(err, boom) || n != 1000 {
			t.Fatalf("WriteTo = %d, %v, want 1000 and the writer's own error", n, err)
		}
		// The writer's failure is not a decode error: the decoder keeps going
		// from the bytes the writer did not take.
		rest, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("ReadAll after a writer error: %v", err)
		}
		requireSame(t, "tail after a writer error", rest, clean[1000:])
	})

	t.Run("short write", func(t *testing.T) {
		d, _ := newSeekable(t, f.data)
		w := &failingWriter{limit: 700}
		n, err := d.WriteTo(w)
		if !errors.Is(err, io.ErrShortWrite) || n != 700 {
			t.Fatalf("WriteTo = %d, %v, want 700 and io.ErrShortWrite", n, err)
		}
		rest, err := io.ReadAll(d)
		if err != nil {
			t.Fatalf("ReadAll after a short write: %v", err)
		}
		requireSame(t, "tail after a short write", rest, clean[700:])
	})
}
