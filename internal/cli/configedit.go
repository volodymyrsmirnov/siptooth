package cli

import (
	"fmt"
	"os"
	"regexp"
)

// phoneAddressLine matches a phone_address line. Only horizontal whitespace is allowed before
// the key and the value part never crosses a newline.
var phoneAddressLine = regexp.MustCompile(`(?m)^([ \t]*phone_address:).*$`)

// setPhoneAddress rewrites the phone_address line of a config file in place, keeping its mode.
func setPhoneAddress(path, address string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !phoneAddressLine.Match(b) {
		return fmt.Errorf("%s has no phone_address line; add bluetooth.phone_address: %q manually", path, address)
	}
	out := phoneAddressLine.ReplaceAll(b, []byte("${1} \""+address+"\""))
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	return os.WriteFile(path, out, mode)
}
