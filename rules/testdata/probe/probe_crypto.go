package probe

// crypto/rand, math/rand and math/rand/v2 all default to the name "rand", so
// each gets its own probe file (this one, probe_randv1.go, probe_randv2.go).
// The split is for readability only: ruleguard resolves a pattern's package
// qualifier through the file's imports, so an aliased import (mrand.Intn)
// matches a "rand.Intn" pattern just the same.

import (
	"crypto/cipher"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"math/big"
)

// rule: DeprecatedCipherModes
func deprecatedCipherModes(block cipher.Block, iv []byte) {
	_ = cipher.NewOFB(block, iv)          // want "cipher.NewOFB is deprecated"
	_ = cipher.NewCFBEncrypter(block, iv) // want "cipher.NewCFBEncrypter is deprecated"
	_ = cipher.NewCFBDecrypter(block, iv) // want "cipher.NewCFBDecrypter is deprecated"
}

// rule: WeakRSAKeySize
func weakRSAKeySize(bits int) {
	_, _ = rsa.GenerateKey(rand.Reader, 1024) // want "RSA keys under 2048 bits are weak"
	_, _ = rsa.GenerateKey(rand.Reader, 512)  // want "RSA keys under 2048 bits are weak"
	_, _ = rsa.GenerateKey(rand.Reader, 768)  // want "RSA keys under 2048 bits are weak"
	_, _ = rsa.GenerateKey(rand.Reader, 1536) // want "RSA keys under 2048 bits are weak"
	// Not flagged: the recommended sizes, and a non-constant size.
	_, _ = rsa.GenerateKey(rand.Reader, 2048)
	_, _ = rsa.GenerateKey(rand.Reader, 4096)
	_, _ = rsa.GenerateKey(rand.Reader, bits)
}

// rule: DeprecatedElliptic
func deprecatedElliptic(curve elliptic.Curve, x, y *big.Int, data []byte) {
	_, _, _, _ = elliptic.GenerateKey(curve, rand.Reader) // want "elliptic.GenerateKey is deprecated"
	_ = elliptic.Marshal(curve, x, y)                     // want "elliptic.Marshal is deprecated"
	_, _ = elliptic.Unmarshal(curve, data)                // want "elliptic.Unmarshal is deprecated"
}

// rule: DeprecatedRSAMultiPrime
func deprecatedRSAMultiPrime(nprimes, bits int) {
	_, _ = rsa.GenerateMultiPrimeKey(rand.Reader, nprimes, bits) // want "rsa.GenerateMultiPrimeKey is deprecated"
}

// Not flagged: crypto/rand.Read is the secure function, not the deprecated
// math/rand.Read. RandV2Migration's rand.Read match is gated on a math/rand
// import, which this file does not have.
func cryptoRandReadNotFlagged(b []byte) {
	_, _ = rand.Read(b)
}

// rule: DeprecatedPKCS1v15
func deprecatedPKCS1v15(pub *rsa.PublicKey, priv *rsa.PrivateKey, msg, ciphertext, key []byte) {
	_, _ = rsa.EncryptPKCS1v15(rand.Reader, pub, msg)                     // want "rsa.EncryptPKCS1v15 is deprecated"
	_, _ = rsa.DecryptPKCS1v15(rand.Reader, priv, ciphertext)             // want "rsa.DecryptPKCS1v15 is deprecated"
	_ = rsa.DecryptPKCS1v15SessionKey(rand.Reader, priv, ciphertext, key) // want "rsa.DecryptPKCS1v15SessionKey is deprecated"
}
