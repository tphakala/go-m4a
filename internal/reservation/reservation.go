// SPDX-License-Identifier: MIT

// Package reservation holds the buffer-reservation policy shared by the codec
// bridges (flacm4a, opusm4a) when an accumulating decode sizes its output slice
// up front. It exists so the rule lives in one discoverable place rather than as
// parallel comments in each bridge, where the two copies drifted apart once.
//
// # The rule
//
// A reservation derived from CALLER input needs only an overflow guard: the
// caller supplied the size, so the only failure to defend against is the
// arithmetic wrapping (a Grow panic, a negative make). The encode paths reserve
// this way (aacm4a's ADTS estimate, flacm4a's frame slice from the caller's PCM
// length).
//
// A reservation derived from FILE input needs more, because the size comes from
// the file's own self-description and the file may be hostile. It needs two
// things: a hard CEILING so a claimed length cannot drive an unbounded
// speculative allocation before a single frame has decoded, and a way to
// CROSS-CHECK the claim against what the container can actually hold (the frame
// count, the per-frame sample limit), so an honest file still gets a useful
// estimate. The decode paths reserve this way and use MaxPCMReservation as the
// ceiling; each bridge's own pcmReservation supplies the container cross-check,
// which is codec-specific and stays with the bridge.
//
// Separately from sizing the reservation, an accumulating decode may hand back a
// slice whose capacity ran far ahead of the audio that decoded (a file that
// over-declared its length). ShouldTrim decides when that dead capacity is worth
// a copy-down; MaxRetainedSlack is its absolute floor. The ceiling bounds the
// worst case, the trim reclaims the common one.
package reservation

// MaxPCMReservation is the ceiling on what an accumulating decode reserves up
// front. It bounds the RESERVATION only; what bounds the decode itself is the
// caller's limit (see m4a.DefaultMaxDecodedBytes), which each bridge's
// pcmReservation also applies. This constant is what keeps a file's own
// self-description from driving a large speculative allocation before the first
// frame has decoded.
//
// The container bounds a bridge's pcmReservation derives (frame count, per-frame
// sample limit) narrow the honest cases but cannot make the reservation safe on
// their own, because a codec's compression ratio has no lower limit: a crafted
// file can buy the largest permitted block for almost nothing, so a couple of
// kilobytes of input can still reach any per-claim estimate. The ceiling is the
// backstop that does not scale with what the file claims.
//
// 64 MiB is about six minutes of 48 kHz stereo 16-bit. The value is a deliberate,
// measured trade. Lowering it to 8 MiB was tried and reverted: it bounds a
// crafted file to 8 MiB instead of 64 MiB, a weak gain given both are transient
// and freed and that the unbounded decode dwarfs either, and it costs every
// honest clip past about 43 seconds its exact reservation. On a three-minute
// stereo clip that gave up essentially all of the benefit, allocating about three
// and a half times what this ceiling does. Honest files are what it is tuned for.
const MaxPCMReservation = 64 << 20

// MaxRetainedSlack is the floor below which an accumulating decode never bothers
// copying the returned slice down to size. It is not the whole rule and not the
// worst case: ShouldTrim also requires the slack to be disproportionate, so what
// can actually be handed back is max(MaxRetainedSlack, length/2), which for a
// buffer near the ceiling is over 20 MiB.
//
// That is a deliberate trade rather than an oversight. Recovering half a buffer
// costs copying all of it, so trimming a 38 MB result to reclaim 19 MB is not
// obviously worth doing, whereas the case this exists for, a file that declared
// orders of magnitude more audio than it carried, clears any such threshold
// easily. The cost is that a moderately over-declared file (a truncated recording
// being the realistic one) keeps proportional slack. See ShouldTrim for why the
// proportional test cannot simply be dropped.
const MaxRetainedSlack = 64 << 10

// ShouldTrim reports whether a decoded buffer of the given length and capacity is
// carrying enough dead capacity to be worth copying down to size.
//
// Both tests are load-bearing. The absolute one ignores small overshoot, which is
// not worth a copy. The proportional one keeps the trim off honest files, and it
// is the whole reason this is a function rather than one condition inline: a
// buffer that reached its length through append carries up to a quarter of that
// length as growth headroom, so an absolute threshold alone fires on essentially
// every stream past a megabyte and charges it a full extra copy. Measured on a
// 9-minute clip, that copy cost about 104 MB, and it fell hardest on streams
// declaring an unknown length, where the reservation never engages at all so the
// copy buys nothing whatsoever.
//
// The divisor is half rather than a quarter deliberately. Growth headroom runs to
// about a quarter of the length, so a quarter is exactly on the boundary and was
// measured still firing on an honest 30-second unknown-length stream. What the
// trim is for is the disproportionate case, where a file declared far more audio
// than it carried and the slack dwarfs the audio instead of being a fraction of
// it; that case clears any of these divisors by orders of magnitude.
func ShouldTrim(length, capacity int) bool {
	slack := capacity - length
	return slack > MaxRetainedSlack && slack > length/2
}
