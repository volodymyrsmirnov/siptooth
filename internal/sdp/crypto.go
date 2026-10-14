package sdp

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
)

// NewKey returns a fresh 30-byte SRTP master key||salt.
func NewKey() ([]byte, error) {
	k := make([]byte, keyLen)
	if _, err := rand.Read(k); err != nil {
		return nil, fmt.Errorf("sdp: generate SRTP key: %w", err)
	}
	return k, nil
}

// parseCrypto parses the value after "a=crypto:". It accepts tag 1, suite
// AES_CM_128_HMAC_SHA1_80 and a 30-byte inline key with an optional lifetime
// (ignored). An MKI or session parameters are unacceptable.
func parseCrypto(v string) ([]byte, error) {
	f := strings.Fields(v)
	if len(f) != 3 {
		return nil, fmt.Errorf("%w: crypto line must have tag, suite and key only", ErrUnacceptable)
	}
	if f[0] != "1" {
		return nil, fmt.Errorf("%w: crypto tag %q (want 1)", ErrUnacceptable, f[0])
	}
	if f[1] != cryptoSuite {
		return nil, fmt.Errorf("%w: crypto suite %q", ErrUnacceptable, f[1])
	}
	rest, ok := strings.CutPrefix(f[2], "inline:")
	if !ok {
		return nil, fmt.Errorf("%w: crypto key method", ErrUnacceptable)
	}
	parts := strings.Split(rest, "|")
	if len(parts) > 2 || strings.Contains(rest, ":") {
		return nil, fmt.Errorf("%w: crypto MKI or extra parameters", ErrUnacceptable)
	}
	key, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil || len(key) != keyLen {
		return nil, fmt.Errorf("%w: crypto key must be %d base64 bytes", ErrUnacceptable, keyLen)
	}
	return key, nil
}

func cryptoLine(key []byte) string {
	return "a=crypto:1 " + cryptoSuite + " inline:" + base64.StdEncoding.EncodeToString(key)
}
