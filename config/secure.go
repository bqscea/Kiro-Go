package config

import "crypto/subtle"

// SecureCompareString performs constant-time string comparison to prevent timing attacks.
// Returns true if a and b are equal, false otherwise.
// This is critical for API key validation where timing differences could leak information
// about the key through side-channel analysis.
//
// Note: Length comparison itself is not constant-time, but this is acceptable for API keys
// which typically have fixed lengths. The primary defense is against timing attacks on the
// byte-by-byte comparison when lengths match.
func SecureCompareString(a, b string) bool {
	// subtle.ConstantTimeCompare requires equal length
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
