package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	got := ParseEnvFile("# comment\n\n  ; also comment\nOTHER=1\nSIPTOOTH_SIP_PASSWORD=\"pw-quoted\"\nSINGLE='x y'\nexport EXPORTED=e\nBROKEN\n SPACED = v \n")
	want := map[string]string{"OTHER": "1", "SIPTOOTH_SIP_PASSWORD": "pw-quoted", "SINGLE": "x y", "EXPORTED": "e", "SPACED": "v"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if ParseEnvFile("K=a\nK=b\n")["K"] != "b" {
		t.Error("last assignment must win")
	}
}

func TestParseEnvFileInlineCommentsAndExportTab(t *testing.T) {
	got := ParseEnvFile("A=value # comment\nB=v\t# tab comment\nC=\"quoted # kept\" # dropped\nD='x#y'\nE=no#comment\nexport\tF=tabbed\nexport  G=spaces\nH= # only a comment\n")
	want := map[string]string{"A": "value", "B": "v", "C": "quoted # kept", "D": "x#y", "E": "no#comment", "F": "tabbed", "G": "spaces", "H": ""}
	for k, v := range want {
		if g, found := got[k]; !found || g != v {
			t.Errorf("%s = %q (found %v), want %q", k, g, found, v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %v", got)
	}
}

func TestEnvFilePath(t *testing.T) {
	if p := EnvFilePath("/home/pi/.siptooth/config.yaml"); p != "/home/pi/.siptooth/env" {
		t.Fatal(p)
	}
}

func TestLookupPassword(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	t.Setenv(PasswordVar, "")
	if pw, err := LookupPassword(cfg); pw != "" || err != nil {
		t.Fatalf("missing file: %q %v", pw, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("# pw\nSIPTOOTH_SIP_PASSWORD='from-file'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if pw, err := LookupPassword(cfg); pw != "from-file" || err != nil {
		t.Fatalf("file: %q %v", pw, err)
	}
	t.Setenv(PasswordVar, "from-env")
	if pw, err := LookupPassword(cfg); pw != "from-env" || err != nil {
		t.Fatalf("env wins: %q %v", pw, err)
	}
}

func TestLookupPasswordUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses permissions")
	}
	dir := t.TempDir()
	t.Setenv(PasswordVar, "")
	if err := os.WriteFile(filepath.Join(dir, "env"), []byte("SIPTOOTH_SIP_PASSWORD=x\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	if _, err := LookupPassword(filepath.Join(dir, "config.yaml")); err == nil {
		t.Fatal("unreadable env file must be an error")
	}
}
