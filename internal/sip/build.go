package sip

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	gosip "github.com/emiago/sipgo/sip"
)

// allowMethods is the Allow header of every INVITE (spec 5.2): no UPDATE,
// no PRACK, no INFO.
const allowMethods = "INVITE, ACK, CANCEL, BYE, OPTIONS"

// buildInvite builds the INVITE by hand (spec 5.2). sipgo's helpers cannot
// set the transport and the next hop independently of the target URI, so the
// request is complete here except for Via and Max-Forwards, which the sipgo
// client adds when it sends. transport is "UDP", "TCP" or "TLS"; nextHop is
// "host:port".
func buildInvite(req DialRequest, transport, nextHop, userAgent string) (*gosip.Request, error) {
	var target gosip.Uri
	if err := gosip.ParseUri(req.Target, &target); err != nil {
		return nil, fmt.Errorf("sip: target %q: %w", req.Target, err)
	}
	if target.Host == "" {
		return nil, fmt.Errorf("sip: target %q has no host", req.Target)
	}
	var from gosip.Uri
	if err := gosip.ParseUri(req.From, &from); err != nil {
		return nil, fmt.Errorf("sip: from %q: %w", req.From, err)
	}
	contact, err := parseContactURI(req.Contact)
	if err != nil {
		return nil, err
	}

	invite := gosip.NewRequest(gosip.INVITE, target)
	invite.SetTransport(transport)
	invite.SetDestination(nextHop)

	fromHdr := &gosip.FromHeader{DisplayName: stripQuotes(req.DisplayName), Address: from}
	fromHdr.Params.Add("tag", gosip.GenerateTagN(16))
	invite.AppendHeader(fromHdr)

	// To carries the target without its URI parameters (transport, headers);
	// the Request-URI keeps them.
	invite.AppendHeader(&gosip.ToHeader{Address: gosip.Uri{
		Scheme: target.Scheme, User: target.User, Host: target.Host, Port: target.Port,
	}})
	callID := gosip.CallIDHeader(gosip.GenerateTagN(24))
	invite.AppendHeader(&callID)
	invite.AppendHeader(&gosip.CSeqHeader{SeqNo: 1, MethodName: gosip.INVITE})
	invite.AppendHeader(&gosip.ContactHeader{Address: contact})
	invite.AppendHeader(gosip.NewHeader("Allow", allowMethods))
	invite.AppendHeader(gosip.NewHeader("User-Agent", userAgent))
	names := make([]string, 0, len(req.Headers))
	for name, value := range req.Headers {
		if name == "" || strings.ContainsAny(name, ": \t") {
			return nil, fmt.Errorf("sip: invalid header name %q", name)
		}
		if strings.ContainsAny(name+value, "\r\n\x00") {
			return nil, fmt.Errorf("sip: header %q has CR, LF or NUL", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		invite.AppendHeader(gosip.NewHeader(name, req.Headers[name]))
	}
	ct := gosip.ContentTypeHeader("application/sdp")
	invite.AppendHeader(&ct)
	invite.SetBody(req.Offer.Marshal())
	return invite, nil
}

// parseContactURI accepts a bare URI or one wrapped in angle brackets.
func parseContactURI(s string) (gosip.Uri, error) {
	var u gosip.Uri
	if s == "" {
		return u, errors.New("sip: contact is required")
	}
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "<"), ">")
	if err := gosip.ParseUri(s, &u); err != nil {
		return u, fmt.Errorf("sip: contact %q: %w", s, err)
	}
	return u, nil
}

// stripQuotes removes one pair of outer double quotes from the quoted string
// placeholder.ExpandDisplayName returns; sipgo adds its own pair when it
// writes the header. Inner backslash escapes stay.
func stripQuotes(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// dialogPath is how requests of a dialog siptooth created are routed (RFC
// 3261 12.2.1.1): the remote target and the Route set, which is the
// Record-Route set of the 2xx reversed. A target refresh replaces target and
// keeps the route set.
type dialogPath struct {
	target gosip.Uri
	routes []gosip.Uri
}

// newDialogPath derives the path from the INVITE and its 2xx: the target is
// the 2xx Contact (else the INVITE's Request-URI).
func newDialogPath(invite *gosip.Request, res *gosip.Response) dialogPath {
	p := dialogPath{target: *invite.Recipient.Clone()}
	if c := res.Contact(); c != nil {
		p.target = *c.Address.Clone()
	}
	var set []gosip.Uri
	for _, h := range res.GetHeaders("record-route") {
		for _, entry := range splitRouteList(h.Value()) {
			var r gosip.RouteHeader
			if err := parseRouteValue(entry, &r); err == nil {
				set = append(set, *r.Address.Clone())
			}
		}
	}
	for i := len(set) - 1; i >= 0; i-- {
		p.routes = append(p.routes, set[i])
	}
	return p
}

// splitRouteList splits a Record-Route header value on the commas outside <>.
func splitRouteList(v string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range v {
		switch r {
		case '<':
			depth++
		case '>':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, v[start:i])
				start = i + 1
			}
		}
	}
	return append(out, v[start:])
}

// parseRouteValue parses one name-addr of a Record-Route value.
func parseRouteValue(v string, h *gosip.RouteHeader) error {
	v = strings.TrimSpace(v)
	if i := strings.Index(v, "<"); i >= 0 {
		j := strings.Index(v[i:], ">")
		if j < 0 {
			return fmt.Errorf("sip: bad route %q", v)
		}
		v = v[i+1 : i+j]
	}
	return gosip.ParseUri(v, &h.Address)
}

// apply sets the Request-URI and Route headers of req. With a loose router
// first in the route set (lr) the target is the Request-URI and the whole set
// goes into Route. A strict router first (no lr) becomes the Request-URI, is
// dropped from Route, and the remote target is appended last.
func (p dialogPath) apply(req *gosip.Request) {
	req.Recipient = *p.target.Clone()
	routes := p.routes
	strict := len(routes) > 0 && !routes[0].UriParams.Has("lr")
	if strict {
		first := *routes[0].Clone()
		first.Headers = nil
		req.Recipient = first
		routes = routes[1:]
	}
	for i := range routes {
		req.AppendHeader(&gosip.RouteHeader{Address: *routes[i].Clone()})
	}
	if strict {
		req.AppendHeader(&gosip.RouteHeader{Address: *p.target.Clone()})
	}
}

// buildAck builds the ACK for a 2xx of invite (RFC 3261 13.2.2.4) outside any
// sipgo session. It is a template: send a Clone, because the client adds a
// Via when it writes. A retransmitted 2xx is answered with another Clone.
func buildAck(invite *gosip.Request, res *gosip.Response) *gosip.Request {
	ack := gosip.NewRequest(gosip.ACK, invite.Recipient)
	newDialogPath(invite, res).apply(ack)
	ack.AppendHeader(gosip.HeaderClone(invite.From()))
	ack.AppendHeader(gosip.HeaderClone(res.To()))
	ack.AppendHeader(gosip.HeaderClone(invite.CallID()))
	ack.AppendHeader(&gosip.CSeqHeader{SeqNo: invite.CSeq().SeqNo, MethodName: gosip.ACK})
	maxFwd := gosip.MaxForwardsHeader(70)
	ack.AppendHeader(&maxFwd)
	ack.SetTransport(invite.Transport())
	ack.Laddr = invite.Laddr
	ack.SetBody(nil)
	return ack
}

// buildBye builds the BYE for the dialog a 2xx of invite established, routed
// by path (the current remote target and the route set). Via is added when
// sent.
func buildBye(invite *gosip.Request, res *gosip.Response, path dialogPath, userAgent string) *gosip.Request {
	bye := gosip.NewRequest(gosip.BYE, invite.Recipient)
	path.apply(bye)
	maxFwd := gosip.MaxForwardsHeader(70)
	bye.AppendHeader(&maxFwd)
	bye.AppendHeader(gosip.HeaderClone(invite.From()))
	bye.AppendHeader(gosip.HeaderClone(res.To()))
	bye.AppendHeader(gosip.HeaderClone(invite.CallID()))
	bye.AppendHeader(&gosip.CSeqHeader{SeqNo: invite.CSeq().SeqNo + 1, MethodName: gosip.BYE})
	if c := invite.Contact(); c != nil {
		bye.AppendHeader(gosip.HeaderClone(c))
	}
	bye.AppendHeader(gosip.NewHeader("User-Agent", userAgent))
	bye.SetTransport(invite.Transport())
	bye.Laddr = invite.Laddr
	bye.SetBody(nil)
	return bye
}

// buildCancel builds the CANCEL of a pending INVITE (RFC 3261 9.1): same
// Request-URI, top Via (branch), From, To, Call-ID, Route set and destination
// as the INVITE, CSeq number with method CANCEL. It is a snapshot: the INVITE
// itself is changed by sipgo when it answers a challenge.
func buildCancel(invite *gosip.Request, userAgent string) *gosip.Request {
	cancel := gosip.NewRequest(gosip.CANCEL, *invite.Recipient.Clone())
	cancel.AppendHeader(gosip.HeaderClone(invite.Via()))
	for _, r := range invite.GetHeaders("Route") {
		cancel.AppendHeader(gosip.HeaderClone(r))
	}
	maxFwd := gosip.MaxForwardsHeader(70)
	cancel.AppendHeader(&maxFwd)
	cancel.AppendHeader(gosip.HeaderClone(invite.From()))
	cancel.AppendHeader(gosip.HeaderClone(invite.To()))
	cancel.AppendHeader(gosip.HeaderClone(invite.CallID()))
	cancel.AppendHeader(&gosip.CSeqHeader{SeqNo: invite.CSeq().SeqNo, MethodName: gosip.CANCEL})
	cancel.AppendHeader(gosip.NewHeader("User-Agent", userAgent))
	cancel.SetTransport(invite.Transport())
	cancel.SetDestination(invite.Destination())
	cancel.Laddr = invite.Laddr
	cancel.SetBody(nil)
	return cancel
}

// toTag returns the To tag of a response, or "".
func toTag(res *gosip.Response) string {
	if to := res.To(); to != nil {
		if tag, ok := to.Params.Get("tag"); ok {
			return tag
		}
	}
	return ""
}
