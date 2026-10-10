package m4a

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// SeekSample 必须落在正确的帧上：跳过若干帧后再读，得到的访问单元
// 与顺序读到的完全一致——这是「seek 只发几个 Range 请求」的正确性前提。
func TestSeekSampleMatchesSequential(t *testing.T) {
	for _, n := range []int{5, 30, 60} {
		frames := synthFrames(n)
		cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}

		seq := readerFromWriter(t, cfg, frames)
		all := collectFrames(t, seq)

		for _, at := range []int{0, 1, n / 2, n - 1} {
			r := readerFromWriter(t, cfg, frames)
			if err := r.SeekSample(at); err != nil {
				t.Fatalf("n=%d SeekSample(%d): %v", n, at, err)
			}
			for i := at; i < n; i++ {
				got, err := r.ReadFrame()
				if err != nil {
					t.Fatalf("n=%d read frame %d after SeekSample(%d): %v", n, i, at, err)
				}
				if !bytes.Equal(got, all[i]) {
					t.Errorf("n=%d SeekSample(%d): frame %d differs", n, at, i)
				}
			}
			if _, err := r.ReadFrame(); !errors.Is(err, io.EOF) {
				t.Errorf("n=%d SeekSample(%d) read past end = %v, want io.EOF", n, at, err)
			}
		}
	}
}

func TestSeekSampleOutOfRange(t *testing.T) {
	frames := synthFrames(5)
	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
	r := readerFromWriter(t, cfg, frames)
	if err := r.SeekSample(-1); err == nil {
		t.Error("SeekSample(-1) should error")
	}
	if err := r.SeekSample(5); err == nil {
		t.Error("SeekSample(5) out of range should error")
	}
	if err := r.SeekSample(0); err != nil {
		t.Errorf("SeekSample(0): %v", err)
	}
	if _, err := r.ReadFrame(); err != nil {
		t.Errorf("read after SeekSample(0): %v", err)
	}
}
