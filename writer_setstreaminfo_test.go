// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestFLACStreamInfoDeferral pins the SetSTREAMINFO contract that lets a FLAC
// encoder stream every frame first and supply the finalized STREAMINFO block
// (with its measured frame sizes and MD5) only at Close. It covers the happy
// path, that the block round-trips into the dfLa box, and every way the deferral
// can be misused.
func TestFLACStreamInfoDeferral(t *testing.T) {
	t.Parallel()

	t.Run("late set then close round-trips the block", func(t *testing.T) {
		t.Parallel()
		si := flacStreamInfo(44100, 1)
		ws := &memWS{}
		w, err := NewWriter(ws, WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, EncoderDelay: NoEdit})
		if err != nil {
			t.Fatalf("NewWriter with empty STREAMINFO should be accepted (deferred), got %v", err)
		}
		for i, au := range synthFrames(3) {
			if err := w.WriteFrameDuration(au, 4096); err != nil {
				t.Fatalf("WriteFrameDuration %d: %v", i, err)
			}
		}
		if err := w.SetSTREAMINFO(si); err != nil {
			t.Fatalf("SetSTREAMINFO: %v", err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		r, err := NewReader(bytes.NewReader(ws.buf))
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		if info := r.Info(); !bytes.Equal(info.CodecConfig, si) {
			t.Errorf("CodecConfig = %x, want the deferred STREAMINFO %x", info.CodecConfig, si)
		}
	})

	t.Run("close without STREAMINFO fails clearly", func(t *testing.T) {
		t.Parallel()
		ws := &memWS{}
		w, err := NewWriter(ws, WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, EncoderDelay: NoEdit})
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if err := w.WriteFrameDuration(synthFrames(1)[0], 4096); err != nil {
			t.Fatalf("WriteFrameDuration: %v", err)
		}
		err = w.Close()
		if err == nil {
			t.Fatal("Close with no STREAMINFO returned nil, want an error")
		}
		if !strings.Contains(err.Error(), "STREAMINFO") {
			t.Errorf("Close error = %q, want it to name STREAMINFO", err)
		}
	})

	t.Run("set after close is rejected with ErrClosed", func(t *testing.T) {
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

	t.Run("set on a non-FLAC writer is rejected", func(t *testing.T) {
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

	t.Run("set with the wrong length is rejected", func(t *testing.T) {
		t.Parallel()
		ws := &memWS{}
		w, err := NewWriter(ws, WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, EncoderDelay: NoEdit})
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if err := w.SetSTREAMINFO(make([]byte, 20)); err == nil {
			t.Error("SetSTREAMINFO with 20 bytes returned nil, want an error")
		}
	})

	t.Run("late set overwrites a block given at NewWriter", func(t *testing.T) {
		t.Parallel()
		early := flacStreamInfo(44100, 1)
		late := flacStreamInfo(48000, 2)
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
		r, err := NewReader(bytes.NewReader(ws.buf))
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		if info := r.Info(); !bytes.Equal(info.CodecConfig, late) {
			t.Errorf("CodecConfig = %x, want the overwritten STREAMINFO %x", info.CodecConfig, late)
		}
	})

	t.Run("caller mutation after set does not change the box", func(t *testing.T) {
		t.Parallel()
		si := flacStreamInfo(44100, 1)
		ws := &memWS{}
		w, err := NewWriter(ws, WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, EncoderDelay: NoEdit})
		if err != nil {
			t.Fatalf("NewWriter: %v", err)
		}
		if err := w.WriteFrameDuration(synthFrames(1)[0], 4096); err != nil {
			t.Fatalf("WriteFrameDuration: %v", err)
		}
		if err := w.SetSTREAMINFO(si); err != nil {
			t.Fatalf("SetSTREAMINFO: %v", err)
		}
		want := bytes.Clone(si)
		for i := range si {
			si[i] ^= 0xFF // mutate the caller's slice after handing it over
		}
		if err := w.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		r, err := NewReader(bytes.NewReader(ws.buf))
		if err != nil {
			t.Fatalf("NewReader: %v", err)
		}
		if info := r.Info(); !bytes.Equal(info.CodecConfig, want) {
			t.Errorf("CodecConfig = %x, want the pre-mutation STREAMINFO %x", info.CodecConfig, want)
		}
	})
}
