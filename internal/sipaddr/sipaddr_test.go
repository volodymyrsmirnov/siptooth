package sipaddr

import (
	"testing"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

func TestNextHop(t *testing.T) {
	cases := []struct {
		name      string
		mod       func(*config.Config)
		want      string
		wantError bool
	}{
		{"tls default port", func(c *config.Config) { c.SIP.Transport, c.SIP.Target = "tls", "sip:{caller}@pbx.example.com" }, "pbx.example.com:5061", false},
		{"udp default port", func(c *config.Config) { c.SIP.Transport, c.SIP.Target = "udp", "sip:agent@pbx.example.com" }, "pbx.example.com:5060", false},
		{"target port", func(c *config.Config) { c.SIP.Target = "sip:agent@10.0.0.5:5080;transport=tcp" }, "10.0.0.5:5080", false},
		{"proxy wins", func(c *config.Config) { c.SIP.Proxy = "proxy.example.com:5070" }, "proxy.example.com:5070", false},
		{"ip literal", func(c *config.Config) { c.SIP.Transport, c.SIP.Target = "udp", "sip:agent@192.0.2.9" }, "192.0.2.9:5060", false},
		{"ipv6", func(c *config.Config) { c.SIP.Target = "sip:agent@[2001:db8::1]:5060" }, "[2001:db8::1]:5060", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.SIP.Target = "sip:x@example.com"
			tc.mod(&cfg)
			got, err := NextHop(cfg)
			if (err != nil) != tc.wantError || got != tc.want {
				t.Fatalf("got %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
