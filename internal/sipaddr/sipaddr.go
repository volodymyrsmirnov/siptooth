// Package sipaddr holds the next-hop rule shared by the daemon and doctor, so
// doctor tests the address the daemon will actually use.
package sipaddr

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	gosip "github.com/emiago/sipgo/sip"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
	"github.com/volodymyrsmirnov/siptooth/internal/placeholder"
)

// NextHop is "host:port" of sip.proxy, else of the sip.target host (default
// port 5061 for TLS, else 5060).
func NextHop(cfg config.Config) (string, error) {
	if cfg.SIP.Proxy != "" {
		return cfg.SIP.Proxy, nil
	}
	uri, err := placeholder.ExpandURI(cfg.SIP.Target, placeholder.Values{Caller: "caller", Number: cfg.Phone.Number, CallID: "call"})
	if err != nil {
		return "", fmt.Errorf("sip.target: %w", err)
	}
	var u gosip.Uri
	if err := gosip.ParseUri(uri, &u); err != nil || u.Host == "" {
		return "", fmt.Errorf("sip.target %q has no host", cfg.SIP.Target)
	}
	port := u.Port
	if port == 0 {
		port = 5060
		if strings.EqualFold(cfg.SIP.Transport, "tls") {
			port = 5061
		}
	}
	host := strings.Trim(u.Host, "[]")
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}
