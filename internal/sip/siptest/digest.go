package siptest

import (
	"crypto/md5" //nolint:gosec // RFC 2617 digest for a test double
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/emiago/sipgo/sip"
)

// Challenge answers req with 401 (or 407 when proxy is true) carrying an MD5
// qop=auth digest challenge for realm and nonce. stale marks the nonce as
// expired while the credentials were right (RFC 2617 3.2.1).
func Challenge(req *sip.Request, tx sip.ServerTransaction, proxy bool, realm, nonce string, stale bool) error {
	code, reason, name := 401, "Unauthorized", "WWW-Authenticate"
	if proxy {
		code, reason, name = 407, "Proxy Authentication Required", "Proxy-Authenticate"
	}
	res := sip.NewResponseFromRequest(req, code, reason, nil)
	v := fmt.Sprintf(`Digest realm=%q, nonce=%q, algorithm=MD5, qop="auth"`, realm, nonce)
	if stale {
		v += ", stale=true"
	}
	res.AppendHeader(sip.NewHeader(name, v))
	return tx.Respond(res)
}

// CheckDigest reports whether req carries an Authorization (or
// Proxy-Authorization) header with a valid MD5 digest response for user,
// pass and nonce.
func CheckDigest(req *sip.Request, user, pass, nonce string) bool {
	h := req.GetHeader("Authorization")
	if h == nil {
		h = req.GetHeader("Proxy-Authorization")
	}
	if h == nil {
		return false
	}
	p := parseDigest(h.Value())
	if p["username"] != user || p["nonce"] != nonce {
		return false
	}
	ha1 := md5hex(user + ":" + p["realm"] + ":" + pass)
	ha2 := md5hex(req.Method.String() + ":" + p["uri"])
	var want string
	if p["qop"] != "" {
		want = md5hex(strings.Join([]string{ha1, nonce, p["nc"], p["cnonce"], p["qop"], ha2}, ":"))
	} else {
		want = md5hex(ha1 + ":" + nonce + ":" + ha2)
	}
	return p["response"] == want
}

func md5hex(s string) string {
	sum := md5.Sum([]byte(s)) //nolint:gosec
	return hex.EncodeToString(sum[:])
}

// parseDigest splits `Digest a=b, c="d"` into a map (values unquoted).
func parseDigest(v string) map[string]string {
	v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), "Digest"))
	out := map[string]string{}
	for _, part := range splitParams(v) {
		k, val, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		out[strings.ToLower(strings.TrimSpace(k))] = strings.Trim(strings.TrimSpace(val), `"`)
	}
	return out
}

// splitParams splits on commas outside double quotes.
func splitParams(s string) []string {
	var out []string
	var b strings.Builder
	quoted := false
	for _, r := range s {
		switch {
		case r == '"':
			quoted = !quoted
			b.WriteRune(r)
		case r == ',' && !quoted:
			out = append(out, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	if b.Len() > 0 {
		out = append(out, b.String())
	}
	return out
}
