package sip

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"strings"
	"time"

	gosip "github.com/emiago/sipgo/sip"
)

// discoverTimeout is the whole budget for DiscoverContact (spec 7.3).
const discoverTimeout = time.Second

// DiscoverContact sends an OPTIONS for target (a SIP URI used as Request-URI)
// to the next hop on the connection the INVITE will use, and returns the
// address the far end saw siptooth's flow come from: the top Via's received
// and rport of the response, whatever its status code (spec 7.3). ok is false
// when the exchange fails, takes longer than one second, or the response has
// no usable received/rport; the caller then falls back to the media address
// IP with the local SIP port.
func (u *UA) DiscoverContact(ctx context.Context, target string) (netip.AddrPort, bool) {
	ap, reason := u.DiscoverContactReason(ctx, target)
	return ap, reason == ""
}

// Reasons DiscoverContactReason gives for not finding an address.
const (
	DiscoverNoAnswer = "no answer"
	DiscoverNoVia    = "no received/rport in Via"
)

// DiscoverContactReason is DiscoverContact with the reason it found nothing:
// "" on success, else DiscoverNoAnswer or DiscoverNoVia.
func (u *UA) DiscoverContactReason(ctx context.Context, target string) (netip.AddrPort, string) {
	var uri gosip.Uri
	if err := gosip.ParseUri(target, &uri); err != nil {
		u.log.Debug("contact discovery: bad target", "error", err)
		return netip.AddrPort{}, DiscoverNoAnswer
	}
	ctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	tp := strings.ToUpper(u.transport)
	dest, err := u.destination(ctx, u.transport, u.opts.NextHop)
	if err != nil {
		u.log.Debug("contact discovery: next hop", "error", err)
		return netip.AddrPort{}, DiscoverNoAnswer
	}
	req := gosip.NewRequest(gosip.OPTIONS, uri)
	req.SetTransport(tp)
	req.SetDestination(dest)

	// A Via with rport on every transport: over TCP/TLS the response only
	// carries the source port when the request asks for it (RFC 3581).
	via := &gosip.ViaHeader{ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: tp}
	via.Params.Add("branch", gosip.GenerateBranchN(16))
	via.Params.Add("rport", "")
	req.AppendHeader(via)

	from := &gosip.FromHeader{Address: gosip.Uri{Scheme: "sip", User: "siptooth", Host: uri.Host}}
	from.Params.Add("tag", gosip.GenerateTagN(16))
	req.AppendHeader(from)
	req.AppendHeader(gosip.NewHeader("User-Agent", u.opts.UserAgent))

	LogMessage(u.log, "send", req)
	res, err := u.client.Do(ctx, req)
	if err != nil {
		if !errors.Is(err, context.DeadlineExceeded) {
			// A timeout leaves the connection (and its local port) in place;
			// any other failure means the flow is broken.
			u.forgetHop(u.opts.NextHop)
		}
		u.log.Debug("contact discovery failed", "error", err)
		return netip.AddrPort{}, DiscoverNoAnswer
	}
	ap, ok := viaFlow(res)
	if !ok {
		return netip.AddrPort{}, DiscoverNoVia
	}
	return ap, ""
}

// viaFlow extracts received/rport from the top Via of a response.
func viaFlow(res *gosip.Response) (netip.AddrPort, bool) {
	via := res.Via()
	if via == nil {
		return netip.AddrPort{}, false
	}
	received, _ := via.Params.Get("received")
	rport, _ := via.Params.Get("rport")
	ip, err := netip.ParseAddr(received)
	if err != nil {
		return netip.AddrPort{}, false
	}
	port, err := strconv.Atoi(rport)
	if err != nil || port < 1 || port > 65535 {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ip.Unmap().WithZone(""), uint16(port)), true
}
