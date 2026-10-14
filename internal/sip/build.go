package sip

import (
	"errors"
	"fmt"
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
	for name, value := range req.Headers {
		invite.AppendHeader(gosip.NewHeader(name, value))
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

// dialogRoute applies RFC 3261 12.2.1.1 for requests of a dialog the UAC
// created: the target is the 2xx Contact (else the INVITE's Request-URI), and
// the Record-Route set of the 2xx, reversed, becomes the Route set.
func dialogRoute(req *gosip.Request, invite *gosip.Request, res *gosip.Response) {
	req.Recipient = invite.Recipient
	if c := res.Contact(); c != nil {
		req.Recipient = *c.Address.Clone()
	}
	rr := res.GetHeaders("record-route")
	for i := len(rr) - 1; i >= 0; i-- {
		req.AppendHeader(gosip.NewHeader("Route", rr[i].Value()))
	}
	if len(rr) > 0 {
		if first := req.Route(); first != nil && !first.Address.UriParams.Has("lr") {
			req.Recipient = first.Address // strict router, same as sipgo
		}
	}
}

// buildAck builds the ACK for a 2xx of invite (RFC 3261 13.2.2.4) without a
// session: it is used for answers sipgo does not hand to the caller, such as
// the second 2xx of a fork. Via is added by the client when it is written.
func buildAck(invite *gosip.Request, res *gosip.Response) *gosip.Request {
	ack := gosip.NewRequest(gosip.ACK, invite.Recipient)
	dialogRoute(ack, invite, res)
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

// buildBye builds the BYE for the dialog a 2xx of invite established. With
// routes false the Route set is left to sipgo's session code, which adds it
// itself (adding it twice would duplicate it). Via is added when sent.
func buildBye(invite *gosip.Request, res *gosip.Response, routes bool, userAgent string) *gosip.Request {
	bye := gosip.NewRequest(gosip.BYE, invite.Recipient)
	if routes {
		dialogRoute(bye, invite, res)
	} else if c := res.Contact(); c != nil {
		bye.Recipient = *c.Address.Clone()
	}
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

// toTag returns the To tag of a response, or "".
func toTag(res *gosip.Response) string {
	if to := res.To(); to != nil {
		if tag, ok := to.Params.Get("tag"); ok {
			return tag
		}
	}
	return ""
}
