package probe

import (
	"math/rand"
	mrand "math/rand"
)

// rule: RandV2Migration
func randV2Migration(n int, n32 int32, n64 int64, seed int64, b []byte) {
	_ = rand.Intn(n)     // want "rand.IntN(n) instead of rand.Intn"
	_ = mrand.Intn(n)    // want "rand.IntN(n) instead of rand.Intn"
	_ = rand.Int31()     // want "rand.Int32() instead of rand.Int31"
	_ = rand.Int31n(n32) // want "rand.Int32N(n32) instead of rand.Int31n"
	_ = rand.Int63()     // want "rand.Int64() instead of rand.Int63"
	_ = rand.Int63n(n64) // want "rand.Int64N(n64) instead of rand.Int63n"
	rand.Seed(seed)      // want "rand.Seed is deprecated"
	_, _ = rand.Read(b)  // want "rand.Read is deprecated"
}
