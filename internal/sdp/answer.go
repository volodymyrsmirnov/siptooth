package sdp

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/volodymyrsmirnov/siptooth/internal/g711"
)

const (
	sectSession = iota
	sectChosen
	sectOther
)

// stream is the first audio stream of a parsed SDP.
type stream struct {
	addr    netip.AddrPort
	profile string
	pts     []int // payload types in m= order
	dir     string
	cryptos []string // values after "a=crypto:"
}

func bad(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrUnacceptable, fmt.Sprintf(format, a...))
}

// parseStream extracts the first m=audio stream. It never panics on garbage.
func parseStream(body []byte) (stream, error) {
	var (
		st                stream
		sessConn, medConn string
		sessDir           string
		inMedia           bool
		sect              = sectSession
		port              int
	)
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if len(line) < 2 || line[1] != '=' {
			continue
		}
		typ, val := line[0], line[2:]
		switch {
		case typ == 'm':
			sect = sectOther
			if inMedia {
				continue
			}
			f := strings.Fields(val)
			if len(f) == 0 || f[0] != "audio" {
				continue
			}
			if len(f) < 4 {
				return st, bad("malformed m=audio line %q", val)
			}
			p, err := strconv.Atoi(f[1])
			if err != nil || p < 0 || p > 65535 {
				return st, bad("media port %q", f[1])
			}
			port = p
			st.profile = f[2]
			for _, s := range f[3:] {
				n, err := strconv.Atoi(s)
				if err != nil || n < 0 || n > 127 {
					return st, bad("payload type %q", s)
				}
				st.pts = append(st.pts, n)
			}
			inMedia = true
			sect = sectChosen
		case sect == sectOther:
		case typ == 'c':
			if sect == sectChosen {
				medConn = val
			} else {
				sessConn = val
			}
		case typ == 'a':
			switch {
			case val == "sendrecv" || val == "sendonly" || val == "recvonly" || val == "inactive":
				if sect == sectChosen {
					st.dir = val
				} else {
					sessDir = val
				}
			case strings.HasPrefix(val, "crypto:") && sect == sectChosen:
				st.cryptos = append(st.cryptos, val[len("crypto:"):])
			}
		}
	}
	if !inMedia {
		return st, bad("no audio stream")
	}
	if port == 0 {
		return st, bad("audio port 0")
	}
	if st.dir == "" {
		st.dir = sessDir
	}
	if st.dir != "" && st.dir != "sendrecv" {
		return st, bad("direction %s", st.dir)
	}
	conn := medConn
	if conn == "" {
		conn = sessConn
	}
	f := strings.Fields(conn)
	if len(f) != 3 || f[0] != "IN" || f[1] != "IP4" {
		return st, bad("connection %q is not IN IP4", conn)
	}
	ip, err := netip.ParseAddr(f[2])
	if err != nil || !ip.Is4() || ip.IsUnspecified() {
		return st, bad("connection address %q", f[2])
	}
	st.addr = netip.AddrPortFrom(ip, uint16(port))
	return st, nil
}

// checkSecurity validates profile and crypto; it returns the remote key (nil
// without SRTP).
func (st stream) checkSecurity(srtp bool) ([]byte, error) {
	if !srtp {
		if st.profile != profileAVP {
			return nil, bad("profile %q (want %s)", st.profile, profileAVP)
		}
		return nil, nil
	}
	if st.profile != profileSAVP {
		return nil, bad("profile %q (want %s)", st.profile, profileSAVP)
	}
	if len(st.cryptos) != 1 {
		return nil, bad("%d crypto lines (want exactly 1)", len(st.cryptos))
	}
	return parseCrypto(st.cryptos[0])
}

func codecOf(pt int) (g711.Codec, bool) {
	switch g711.Codec(pt) {
	case g711.PCMU:
		return g711.PCMU, true
	case g711.PCMA:
		return g711.PCMA, true
	}
	return 0, false
}

// ParseAnswer validates the far end's answer to offer o (spec section 6.4).
func ParseAnswer(body []byte, o Offer) (Media, error) {
	st, err := parseStream(body)
	if err != nil {
		return Media{}, err
	}
	srtp := o.LocalKey != nil
	key, err := st.checkSecurity(srtp)
	if err != nil {
		return Media{}, err
	}
	offered := map[g711.Codec]bool{}
	for _, c := range o.Codecs {
		offered[c] = true
	}
	inAnswer := map[g711.Codec]bool{}
	m := Media{Addr: st.addr, SRTP: srtp, RemoteKey: key}
	found := false
	for _, pt := range st.pts {
		c, ok := codecOf(pt)
		if !ok || !offered[c] {
			continue
		}
		inAnswer[c] = true
		if !found {
			m.SendCodec, found = c, true
		}
	}
	if !found {
		return Media{}, bad("no offered payload type in answer")
	}
	for _, c := range o.Codecs {
		if inAnswer[c] {
			m.Recv = append(m.Recv, c)
		}
	}
	return m, nil
}

// AnswerFor renders our answer to a re-offer: only m.SendCodec, version
// o.Version (the caller increments it), o.Addr, and our key when SRTP.
func AnswerFor(o Offer, m Media) []byte {
	a := o
	a.Codecs = []g711.Codec{m.SendCodec}
	if !m.SRTP {
		a.LocalKey = nil
	}
	return a.Marshal()
}
