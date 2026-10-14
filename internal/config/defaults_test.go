package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsValid(t *testing.T) {
	yml := string(DefaultYAML)
	for _, s := range []string{`phone_address: ""`, `number: ""`} {
		if !strings.Contains(yml, s) {
			t.Fatalf("default yaml lacks %s", s)
		}
	}
	yml = strings.Replace(yml, `phone_address: ""`, `phone_address: "AA:BB:CC:DD:EE:FF"`, 1)
	yml = strings.Replace(yml, `number: ""`, `number: "+380441234567"`, 1)
	p := write(t, t.TempDir(), yml)
	c, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if errs := Validate(c, ""); len(errs) != 0 {
		t.Fatal(errs)
	}
	// The embedded file and Default() agree on every default except those Load resolves.
	d := Default()
	d.Bluetooth.PhoneAddress, d.Phone.Number = c.Bluetooth.PhoneAddress, c.Phone.Number
	d.SIP.Target = c.SIP.Target
	d.Control.Socket = c.Control.Socket
	if !equalConfig(c, d) {
		t.Fatalf("embedded yaml differs from Default():\n%+v\n%+v", c, d)
	}
}

func TestDefaultsRequireUserInput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), bytes.Clone(DefaultYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	got := problems(c, "")
	if !strings.Contains(got, "siptooth pair") || !strings.Contains(got, "phone.number") {
		t.Fatalf("untouched defaults must ask for the phone address and number: %s", got)
	}
}
