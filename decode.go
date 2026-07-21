// SPDX-License-Identifier: MIT

package m4a

// DefaultMaxDecodedBytes is the ceiling the codec bridges apply to a decode that
// accumulates its output into one slice: flacm4a.DecodeInterleaved and
// opusm4a.DecodeInterleaved stop and return ErrDecodeLimit rather than hand back
// more than this. The DecodeInterleavedLimit variants take an explicit limit
// instead, and DecodeStream is bounded by construction because it never
// accumulates.
//
// A limit is needed because the decoded size is not proportional to the file.
// The container's own parsing is bounded by the real file length, so the number
// of access units an attacker gets is bounded by what they pay for, but what an
// access unit decodes to is not: a FLAC constant subframe encodes a whole
// 65535-sample block in a handful of bytes, and a two-byte Opus packet with
// zero-length DTX frames decodes to 120 ms. FLAC amplifies by four to five
// orders of magnitude, so a crafted file of tens of kilobytes decodes to
// something on the order of a gigabyte; Opus is less exposed at three to four
// orders, because a packet decodes to at most 5760 samples per channel and each
// one costs a sample-table entry as well, but the loop shape is identical and
// the bound is worth having in both. Nor is this only an adversarial concern:
// sixty seconds of ordinary digital silence encodes to about 14 kB of FLAC and
// decodes back to 11.5 MB, a ratio of 831:1, so a quiet field recording lands in
// the same place as a crafted one.
//
// 1 GiB is deliberately generous, because this is a backstop against
// amplification rather than a policy on how long a clip may be. It has to sit
// above the largest honest input, and honest inputs run long: 1 GiB is about 101
// minutes of CD-quality 44.1 kHz stereo 16-bit, which covers essentially every
// album, podcast and lecture recording, about 93 minutes of the same at 48 kHz,
// about 31 minutes of 24-bit 96 kHz stereo, and about 15 minutes of 48 kHz
// 8-channel 24-bit. Being generous costs little, because what an attacker has to
// supply to reach the ceiling stays small either way: the cheapest maximal FLAC
// access unit is on the order of fifty bytes and decodes to about two megabytes,
// so tens of kilobytes of crafted input reach 1 GiB just as they reached a
// quarter of it.
//
// It is not a memory guarantee, and a caller that needs one wants DecodeStream.
// An accumulating decode grows geometrically (about 1.25x once the buffer is
// large, not 2x), so while the old array is still live and the new one is being
// filled the transient peak is close to twice the length reached, and the total
// allocated over the whole decode, most of it garbage, is around five times it.
const DefaultMaxDecodedBytes = 1 << 30
