package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestVersionCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"version"}, &out, &errOut); code != ExitOK {
		t.Fatalf("exit code %d, stderr %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "siptooth") {
		t.Fatalf("output %q does not mention siptooth", out.String())
	}
}

func TestUnknownCommandIsUsageError(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"nope"}, &out, &errOut); code != ExitUsage {
		t.Fatalf("exit code %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(errOut.String(), "unknown command") {
		t.Fatalf("stderr %q", errOut.String())
	}
}
