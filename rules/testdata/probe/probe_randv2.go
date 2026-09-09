package probe

import (
	"math/rand/v2"
	"time"
)

// rule: RandMethodN
func randMethodN(r *rand.Rand, d time.Duration, n int, u uint) {
	_ = time.Duration(r.Int64N(int64(d))) // want "use r.N(d) instead of converting"
	_ = int32(r.Int32N(int32(n)))         // not flagged: int32(...) is not the type of n
	_ = int(r.IntN(int(n)))               // want "use r.N(n) instead of converting"
	_ = uint(r.UintN(uint(u)))            // want "use r.N(u) instead of converting"
	// Not flagged: the global functions have had rand.N since Go 1.22.
	_ = time.Duration(rand.Int64N(int64(d)))
}
