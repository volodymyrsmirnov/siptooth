package config

import _ "embed"

// DefaultYAML is the commented default config.yaml that `siptooth install` writes. It has
// no phone address, so it does not validate until `siptooth pair` fills that in.
//
//go:embed defaults/config.yaml
var DefaultYAML []byte
