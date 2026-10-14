package main

import (
	"net"
	"os"

	"github.com/volodymyrsmirnov/siptooth/internal/cli"
	"github.com/volodymyrsmirnov/siptooth/internal/dnsv4"
)

func main() {
	// IPv4-only DNS for the whole process (spec 6.4/7): sipgo, TLS dials and
	// doctor all use net.DefaultResolver.
	net.DefaultResolver = dnsv4.Resolver(nil)
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
