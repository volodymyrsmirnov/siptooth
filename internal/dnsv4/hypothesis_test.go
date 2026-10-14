package dnsv4_test

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestStockGoResolverVersusMagicDNS records how Go's pure resolver reacts to
// an NXDOMAIN AAAA answer next to a successful A answer. It only logs. Result
// on Go 1.24: NXDOMAIN (with or without the authoritative bit) does NOT break
// the lookup, the A address is returned; a dropped AAAA gives an i/o timeout,
// not "no such host" (omitted here, it takes the whole context deadline).
// So plain NXDOMAIN-on-AAAA does not explain the Pi failure by itself.
func TestStockGoResolverVersusMagicDNS(t *testing.T) {
	for _, tc := range []struct {
		name string
		mode aaaaMode
	}{{"nxdomain", aaaaNXDomain}, {"nxdomain+aa", aaaaNXDomainAA}} {
		t.Run(tc.name, func(t *testing.T) {
			f := startFakeDNS(t, tc.mode, map[string]net.IP{"sip.test.": net.ParseIP("136.112.48.140")}, nil)
			r := &net.Resolver{PreferGo: true, Dial: dialTo(f.addr)}
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			start := time.Now()
			ips, err := r.LookupIPAddr(ctx, "sip.test")
			t.Logf("LookupIPAddr: %v err=%v after %v", ips, err, time.Since(start).Round(time.Millisecond))
			nips, err := r.LookupNetIP(ctx, "ip", "sip.test")
			t.Logf("LookupNetIP(ip): %v err=%v", nips, err)
		})
	}
}
