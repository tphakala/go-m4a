// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// The SetSTREAMINFO tests pin the contract that lets a FLAC encoder stream every
// frame first and supply the finalized STREAMINFO block (with its measured frame
// sizes and MD5) only at Close.

// newDeferredFLACWriter creates a CodecFLAC writer with an empty (deferred)
// STREAMINFO and writes one frame, the minimal state for exercising SetSTREAMINFO.
func newDeferredFLACWriter(t *testing.T, ws *memWS) *Writer {
	t.Helper()
	w, err := NewWriter(ws, WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, EncoderDelay: NoEdit})
	if err != nil {
		t.Fatalf("NewWriter with empty STREAMINFO should be accepted (deferred), got %v", err)
	}
	if err := w.WriteFrameDuration(synthFrames(1)[0], 4096); err != nil {
		t.Fatalf("WriteFrameDuration: %v", err)
	}
	return w
}

// readCodecConfig closes ws-backed output and returns the read-back CodecConfig.
func readCodecConfig(t *testing.T, ws *memWS) []byte {
	t.Helper()
	r, err := NewReader(bytes.NewReader(ws.buf))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return r.Info().CodecConfig
}

// TestFLACStreamInfoLateSetRoundTrips writes frames, supplies STREAMINFO late, and
// confirms the block reaches the dfLa box.
func TestFLACStreamInfoLateSetRoundTrips(t *testing.T) {
	t.Parallel()
	si := flacStreamInfo(44100, 1)
	ws := &memWS{}
	w := newDeferredFLACWriter(t, ws)
	if err := w.SetSTREAMINFO(si); err != nil {
		t.Fatalf("SetSTREAMINFO: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := readCodecConfig(t, ws); !bytes.Equal(got, si) {
		t.Errorf("CodecConfig = %x, want the deferred STREAMINFO %x", got, si)
	}
}

// TestFLACStreamInfoLateSetOverwrites confirms a late SetSTREAMINFO replaces a
// block that was given at NewWriter.
func TestFLACStreamInfoLateSetOverwrites(t *testing.T) {
	t.Parallel()
	early := flacStreamInfo(44100, 1)
	// The late block must agree with the writer's configured rate and channel count
	// (SetSTREAMINFO now cross-checks them), so it differs from early only in a
	// non-validated field (a byte of the MD5 signature) to keep the overwrite
	// observable.
	late := flacStreamInfo(44100, 1)
	late[20] = 0xAB
	ws := &memWS{}
	w, err := NewWriter(ws, WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: early, EncoderDelay: NoEdit})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteFrameDuration(synthFrames(1)[0], 4096); err != nil {
		t.Fatalf("WriteFrameDuration: %v", err)
	}
	if err := w.SetSTREAMINFO(late); err != nil {
		t.Fatalf("SetSTREAMINFO: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := readCodecConfig(t, ws); !bytes.Equal(got, late) {
		t.Errorf("CodecConfig = %x, want the overwritten STREAMINFO %x", got, late)
	}
}

// TestFLACStreamInfoCopiesBytes confirms SetSTREAMINFO copies the block, so a later
// caller mutation of the slice cannot change the written box.
func TestFLACStreamInfoCopiesBytes(t *testing.T) {
	t.Parallel()
	si := flacStreamInfo(44100, 1)
	want := bytes.Clone(si)
	ws := &memWS{}
	w := newDeferredFLACWriter(t, ws)
	if err := w.SetSTREAMINFO(si); err != nil {
		t.Fatalf("SetSTREAMINFO: %v", err)
	}
	for i := range si {
		si[i] ^= 0xFF // mutate the caller's slice after handing it over
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := readCodecConfig(t, ws); !bytes.Equal(got, want) {
		t.Errorf("CodecConfig = %x, want the pre-mutation STREAMINFO %x", got, want)
	}
}

// TestFLACCloseWithoutStreamInfo confirms Close fails clearly when a deferred FLAC
// track never got SetSTREAMINFO, rather than writing a malformed dfLa.
func TestFLACCloseWithoutStreamInfo(t *testing.T) {
	t.Parallel()
	ws := &memWS{}
	w := newDeferredFLACWriter(t, ws)
	err := w.Close()
	if err == nil {
		t.Fatal("Close with no STREAMINFO returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "STREAMINFO") {
		t.Errorf("Close error = %q, want it to name STREAMINFO", err)
	}
}

// TestFLACSetStreamInfoRejects covers the misuse cases: after Close, on a non-FLAC
// writer, and with a wrong-length block.
func TestFLACSetStreamInfoRejects(t *testing.T) {
	t.Parallel()

	t.Run("after close returns ErrClosed", func(t *testing.T) {
		t.Parallel()
		si := flacStreamInfo(48000, 2)
		ws := &memWS{}
		w, err := NewWriter(ws, WriterConfig{Codec: CodecFLAC, SampleRate: 48000, Channels: 2, STREAMINFO: si, EncoderDelay: NoEdit})
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if err := w.WriteFrameDuration(synthFrames(1)[0], 4096); err != nil {
			t.Fatalf("WriteFrameDuration: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := w.SetSTREAMINFO(si); !errors.Is(err, ErrClosed) {
			t.Errorf("SetSTREAMINFO after Close = %v, want ErrClosed", err)
		}
	})

	t.Run("non-FLAC writer is rejected", func(t *testing.T) {
		t.Parallel()
		ws := &memWS{}
		w, err := NewWriter(ws, WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 1})
		if err != nil {
			t.Fatalf("NewWriter (Opus): %v", err)
		}
		if err := w.SetSTREAMINFO(flacStreamInfo(48000, 1)); err == nil {
			t.Error("SetSTREAMINFO on an Opus writer returned nil, want an error")
		}
	})

	t.Run("wrong length is rejected", func(t *testing.T) {
		t.Parallel()
		ws := &memWS{}
		w := newDeferredFLACWriter(t, ws)
		if err := w.SetSTREAMINFO(make([]byte, 20)); err == nil {
			t.Error("SetSTREAMINFO with 20 bytes returned nil, want an error")
		}
	})
}

// TestFLACStreamInfoValidation pins the #37 cross-check: a STREAMINFO whose declared
// sample rate or channel count disagrees with the writer config is rejected at both
// entry points (SetSTREAMINFO and the initial WriterConfig.STREAMINFO), an all-zero
// placeholder still passes, and a matching block (including multichannel) is accepted.
func TestFLACStreamInfoValidation(t *testing.T) {
	t.Parallel()

	t.Run("SetSTREAMINFO rejects a sample-rate mismatch", func(t *testing.T) {
		t.Parallel()
		ws := &memWS{}
		w := newDeferredFLACWriter(t, ws) // configured 44100 Hz / 1 ch
		err := w.SetSTREAMINFO(flacStreamInfo(48000, 1))
		if err == nil || !strings.Contains(err.Error(), "sample rate") {
			t.Errorf("SetSTREAMINFO(48000/1) error = %v, want a sample-rate mismatch", err)
		}
	})

	t.Run("SetSTREAMINFO rejects a channel mismatch", func(t *testing.T) {
		t.Parallel()
		ws := &memWS{}
		w := newDeferredFLACWriter(t, ws) // configured 44100 Hz / 1 ch
		err := w.SetSTREAMINFO(flacStreamInfo(44100, 2))
		if err == nil || !strings.Contains(err.Error(), "channel") {
			t.Errorf("SetSTREAMINFO(44100/2) error = %v, want a channel mismatch", err)
		}
	})

	t.Run("SetSTREAMINFO accepts an all-zero placeholder", func(t *testing.T) {
		t.Parallel()
		ws := &memWS{}
		w := newDeferredFLACWriter(t, ws)
		// A rate-0 block carries no rate to check, so it passes: this is what the
		// streaming path writes before the finalized block arrives.
		if err := w.SetSTREAMINFO(make([]byte, flacStreamInfoLen)); err != nil {
			t.Errorf("SetSTREAMINFO(placeholder) = %v, want nil", err)
		}
	})

	t.Run("SetSTREAMINFO rejects a non-placeholder zero-rate block", func(t *testing.T) {
		t.Parallel()
		ws := &memWS{}
		w := newDeferredFLACWriter(t, ws) // configured 44100 Hz / 1 ch
		// Only the all-zero block is the accepted placeholder. A block whose sample
		// rate is 0 but which carries other data (here the channel field) is malformed
		// and must not bypass the cross-check on its zero rate.
		err := w.SetSTREAMINFO(flacStreamInfo(0, 2))
		if err == nil || !strings.Contains(err.Error(), "sample rate") {
			t.Errorf("SetSTREAMINFO(rate 0, 2 ch) error = %v, want a sample-rate mismatch", err)
		}
	})

	t.Run("SetSTREAMINFO accepts a matching block", func(t *testing.T) {
		t.Parallel()
		ws := &memWS{}
		w := newDeferredFLACWriter(t, ws)
		if err := w.SetSTREAMINFO(flacStreamInfo(44100, 1)); err != nil {
			t.Errorf("SetSTREAMINFO(44100/1) = %v, want nil", err)
		}
	})

	t.Run("NewWriter rejects a mismatched initial STREAMINFO", func(t *testing.T) {
		t.Parallel()
		ws := &memWS{}
		_, err := NewWriter(ws, WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: flacStreamInfo(48000, 1), EncoderDelay: NoEdit})
		if err == nil || !strings.Contains(err.Error(), "sample rate") {
			t.Errorf("NewWriter with mismatched STREAMINFO error = %v, want a sample-rate mismatch", err)
		}
	})

	t.Run("NewWriter accepts a matching multichannel STREAMINFO at the 8-channel boundary", func(t *testing.T) {
		t.Parallel()
		ws := &memWS{}
		// 8 is the exact accepted FLAC boundary (maxFLACChannels); exercise it through
		// validateFLACConfig so an off-by-one (> vs >=) regression is caught here, not
		// only in the flacm4a bridge.
		w, err := NewWriter(ws, WriterConfig{Codec: CodecFLAC, SampleRate: 48000, Channels: 8, STREAMINFO: flacStreamInfo(48000, 8), EncoderDelay: NoEdit})
		if err != nil {
			t.Fatalf("NewWriter with a matching 8-channel STREAMINFO: %v", err)
		}
		if err := w.WriteFrameDuration(synthFrames(1)[0], 4096); err != nil {
			t.Fatalf("WriteFrameDuration: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}
