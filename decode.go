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
// zero-length DTX frames decodes to 120 ms. Both amplify by four to five orders
// of magnitude, so a file of tens of kilobytes decodes to something on the order
// of a gigabyte. Nor is this only an adversarial concern: sixty seconds of
// ordinary digital silence encodes to about 14 kB of FLAC and decodes to 11.5 MB,
// a ratio of 831:1, so a quiet field recording lands in the same place.
//
// 256 MiB is about 23 minutes of 48 kHz stereo 16-bit. It is deliberately
// generous, because it is a backstop against amplification rather than a policy
// on how long a clip may be: an honest recording that trips it is a recording
// nobody should be decoding into a single slice anyway. The accumulating decode
// grows by doubling, so the transient peak while a decode approaches this ceiling
// is larger than the ceiling itself; a caller that has to bound its real memory
// use wants DecodeStream, not a smaller limit.
const DefaultMaxDecodedBytes = 256 << 20
