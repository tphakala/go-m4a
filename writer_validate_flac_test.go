package m4a

import (
	"strings"
	"testing"
)

// TestValidateFLACConfig exercises the FLAC config validator directly. CodeGraph
// reported it and validateFlacStreamInfo as having no covering tests, yet they are
// the gate that keeps a dfLa from disagreeing with its sample entry, so pin every
// branch here rather than only through the writer's round-trip tests.
func TestValidateFLACConfig(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		cfg     WriterConfig
		wantErr string // substring; "" means the config must be accepted
	}{
		{
			name: "eight channels at the FLAC maximum",
			cfg:  WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: maxFLACChannels, STREAMINFO: flacStreamInfo(44100, maxFLACChannels)},
		},
		{
			name:    "nine channels above the FLAC maximum",
			cfg:     WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: maxFLACChannels + 1},
			wantErr: "out of range for FLAC",
		},
		{
			name: "sample rate at the STREAMINFO maximum",
			cfg:  WriterConfig{Codec: CodecFLAC, SampleRate: maxFLACSampleRate, Channels: 1, STREAMINFO: flacStreamInfo(maxFLACSampleRate, 1)},
		},
		{
			name:    "sample rate above the STREAMINFO maximum",
			cfg:     WriterConfig{Codec: CodecFLAC, SampleRate: maxFLACSampleRate + 1, Channels: 1},
			wantErr: "exceeds the maximum",
		},
		{
			name:    "STREAMINFO of the wrong length",
			cfg:     WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: make([]byte, 20)},
			wantErr: "want 34 or 0",
		},
		{
			name: "empty STREAMINFO deferred to SetSTREAMINFO",
			cfg:  WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: nil},
		},
		{
			name: "all-zero placeholder accepted on the plain path",
			cfg:  WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: make([]byte, flacStreamInfoLen)},
		},
		{
			name: "populated STREAMINFO agreeing with the config",
			cfg:  WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: flacStreamInfo(44100, 1)},
		},
		{
			name:    "populated STREAMINFO sample rate disagreeing",
			cfg:     WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: flacStreamInfo(48000, 1)},
			wantErr: "sample rate",
		},
		{
			name:    "populated STREAMINFO channel count disagreeing",
			cfg:     WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: flacStreamInfo(44100, 2)},
			wantErr: "channel count",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateFLACConfig(tc.cfg)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validateFLACConfig accepted a valid config, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validateFLACConfig accepted an invalid config, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestValidateFlacStreamInfo pins the cross-check helper directly, including the two
// no-op cases (non-34-byte length and the all-zero placeholder) and the malformed
// zero-rate block that must NOT be mistaken for the placeholder. Messages here are
// not "go-m4a:"-prefixed by design; the callers add the package prefix.
func TestValidateFlacStreamInfo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		streamInfo []byte
		channels   int
		sampleRate int
		wantErr    string
	}{
		{
			name:       "wrong length is a no-op",
			streamInfo: make([]byte, 20),
			channels:   1,
			sampleRate: 44100,
		},
		{
			name:       "all-zero placeholder is a no-op",
			streamInfo: make([]byte, flacStreamInfoLen),
			channels:   1,
			sampleRate: 44100,
		},
		{
			name:       "populated block agreeing with the config",
			streamInfo: flacStreamInfo(44100, 1),
			channels:   1,
			sampleRate: 44100,
		},
		{
			name:       "sample rate disagreeing",
			streamInfo: flacStreamInfo(48000, 2),
			channels:   2,
			sampleRate: 44100,
			wantErr:    "sample rate",
		},
		{
			name:       "channel count disagreeing",
			streamInfo: flacStreamInfo(44100, 2),
			channels:   1,
			sampleRate: 44100,
			wantErr:    "channel count",
		},
		{
			name:       "zero-rate block that is not the placeholder",
			streamInfo: flacStreamInfo(0, 2), // rate 0 but channel bits set, so not all-zero
			channels:   2,
			sampleRate: 44100,
			wantErr:    "0 Hz disagrees", // the rate-0 block is cross-checked, not treated as the placeholder
		},
		{
			name:       "populated block agreeing at eight channels",
			streamInfo: flacStreamInfo(96000, 8),
			channels:   8,
			sampleRate: 96000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateFlacStreamInfo(tc.streamInfo, tc.channels, tc.sampleRate)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validateFlacStreamInfo rejected an allowed block, got %v", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validateFlacStreamInfo accepted a bad block, want error containing %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}

// TestIsDeferredFLACStreamInfo covers the shared predicate both the plain-path
// accept site and the fragmented-path reject site branch on.
func TestIsDeferredFLACStreamInfo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		streamInfo []byte
		want       bool
	}{
		{"all-zero 34-byte placeholder", make([]byte, flacStreamInfoLen), true},
		{"empty slice", nil, false},
		{"wrong length all-zero", make([]byte, 20), false},
		{"populated block", flacStreamInfo(44100, 1), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isDeferredFLACStreamInfo(tc.streamInfo); got != tc.want {
				t.Errorf("isDeferredFLACStreamInfo = %v, want %v", got, tc.want)
			}
		})
	}
}
