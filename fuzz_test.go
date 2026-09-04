// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// isReaderSentinel reports whether err is one of the errors the Reader is allowed
// to return: a wrapped ErrCorrupt or ErrUnsupported for rejected input, a wrapped
// ErrBoxTooLarge for a box body over the buffer limit, or io.EOF at the clean end
// of the frames. Any other error from a Reader that NewReader accepted is a
// contract violation the fuzzer must surface.
func isReaderSentinel(err error) bool {
	return errors.Is(err, ErrCorrupt) || errors.Is(err, ErrUnsupported) ||
		errors.Is(err, ErrBoxTooLarge) || errors.Is(err, io.EOF)
}

// writerSeed builds a valid in-memory M4A from a handful of synthetic access
// units, so the fuzz corpus starts from at least one file the Writer itself
// produced. It returns ok=false rather than failing if the Writer path errors,
// so a seed hiccup never breaks corpus setup.
func writerSeed() (data []byte, ok bool) {
	ws := &memWS{}
	w, err := NewWriter(ws, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k})
	if err != nil {
		return nil, false
	}
	for _, au := range synthFrames(6) {
		if err := w.WriteFrame(au); err != nil {
			return nil, false
		}
	}
	if err := w.Close(); err != nil {
		return nil, false
	}
	return ws.buf, true
}

// fragmentedSeed builds a valid fragmented (CMAF) stream, an init segment plus two
// media segments, so the fuzz corpus reaches the moof/traf/tfhd/trun demux path
// that a plain-file seed never constructs.
func fragmentedSeed() (data []byte, ok bool) {
	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
	init, err := InitSegment(cfg)
	if err != nil {
		return nil, false
	}
	fw, err := NewFragmentWriter(cfg)
	if err != nil {
		return nil, false
	}
	out := append([]byte(nil), init...)
	for _, seg := range [][]byte{{0x21, 0x22, 0x23}, {0x31, 0x32}} {
		for range 3 {
			if err := fw.WriteFrameDuration(seg, 1024); err != nil {
				return nil, false
			}
		}
		if out, err = fw.AppendSegment(out); err != nil {
			return nil, false
		}
	}
	return out, true
}

// FuzzReader is the continuous demuxer fuzz target the CI workflow drives. It
// asserts a single safety contract: over any input, NewReader plus a full
// ReadFrame/RawStream drain never panics, and every error surfaced is a wrapped
// ErrCorrupt, ErrUnsupported, or io.EOF. A crash, or any other error class, is a
// real reader bug.
func FuzzReader(f *testing.F) {
	// Seed with the interop fixtures. A missing or unreadable fixture is skipped,
	// not fatal, so the fuzzer still runs in a trimmed checkout. Both the AAC-LC
	// fixtures and the Opus/FLAC fixtures are seeded so the fuzzer reaches the dOps
	// and dfLa parsers, which mutating an AAC seed would essentially never construct.
	seedNames := make([]string, 0, len(interopFiles)+len(opusFlacFixtures))
	for _, tc := range interopFiles {
		seedNames = append(seedNames, tc.name)
	}
	for _, tc := range opusFlacFixtures {
		seedNames = append(seedNames, tc.name)
	}
	for _, name := range seedNames {
		path := filepath.Join("testdata", "interop", name)
		if b, err := os.ReadFile(path); err == nil { //nolint:gosec // fixed test fixture path
			f.Add(b)
		}
	}
	// Seed with one file the Writer produced from synthetic access units.
	if b, ok := writerSeed(); ok {
		f.Add(b)
	}
	// Seed with a fragmented (CMAF) stream so the fuzzer exercises the demux path.
	if b, ok := fragmentedSeed(); ok {
		f.Add(b)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("panic on %d-byte input % x: %v", len(data), data, p)
			}
		}()

		r, err := NewReader(bytes.NewReader(data))
		if err != nil {
			// A rejected input is a valid outcome, but the rejection must carry a
			// typed sentinel, exactly as the ReadFrame/RawStream errors below are
			// checked. A bare or mistyped error from NewReader is a contract break.
			if !isReaderSentinel(err) {
				t.Errorf("NewReader error = %v, want ErrCorrupt/ErrUnsupported/io.EOF", err)
			}
			return
		}

		_ = r.Info()

		// Drain the access units. ReadFrame ends at a non-nil error, which must be
		// io.EOF or a wrapped corruption/unsupported sentinel.
		for {
			_, ferr := r.ReadFrame()
			if ferr != nil {
				if !isReaderSentinel(ferr) {
					t.Errorf("ReadFrame error = %v, want ErrCorrupt/ErrUnsupported/io.EOF", ferr)
				}
				break
			}
		}

		// Drain the length-prefixed raw stream, which shares the Reader cursor.
		// io.Copy folds io.EOF into a nil return, so only a real error remains to
		// check against the sentinels.
		if _, cerr := io.Copy(io.Discard, r.RawStream()); cerr != nil && !isReaderSentinel(cerr) {
			t.Errorf("RawStream error = %v, want ErrCorrupt/ErrUnsupported/io.EOF", cerr)
		}
	})
}
