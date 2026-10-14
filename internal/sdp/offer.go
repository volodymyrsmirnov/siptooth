package sdp

import (
	"encoding/base64"
	"github.com/volodymyrsmirnov/siptooth/internal/g711"
	"strconv"
	"strings"
)

// Marshal renders the offer with CRLF line endings.
func (o Offer) Marshal() []byte {
	return o.render(o.Codecs)
}

func (o Offer) render(codecs []g711.Codec) []byte {
	ip := o.Addr.Addr().String()
	profile := profileAVP
	if o.LocalKey != nil {
		profile = profileSAVP
	}
	var pts []string
	for _, c := range codecs {
		pts = append(pts, strconv.Itoa(int(c)))
	}
	lines := []string{
		"v=0",
		"o=siptooth " + strconv.FormatUint(o.SessID, 10) + " " + strconv.FormatUint(o.Version, 10) + " IN IP4 " + ip,
		"s=siptooth",
		"c=IN IP4 " + ip,
		"t=0 0",
		"m=audio " + strconv.Itoa(int(o.Addr.Port())) + " " + profile + " " + strings.Join(pts, " "),
	}
	for _, c := range codecs {
		lines = append(lines, "a=rtpmap:"+strconv.Itoa(int(c))+" "+c.String()+"/8000")
	}
	lines = append(lines, "a=ptime:20", "a=sendrecv")
	if o.LocalKey != nil {
		lines = append(lines, "a=crypto:1 "+cryptoSuite+" inline:"+base64.StdEncoding.EncodeToString(o.LocalKey))
	}
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}
