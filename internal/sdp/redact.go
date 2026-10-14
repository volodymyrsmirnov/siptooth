package sdp

import "regexp"

var inlineKey = regexp.MustCompile(`(?i)inline:\S*`)

// Redact replaces every SDES inline key (including lifetime/MKI suffixes)
// with inline:<redacted> so SDP can be logged.
func Redact(body []byte) []byte {
	return inlineKey.ReplaceAll(body, []byte("inline:<redacted>"))
}
