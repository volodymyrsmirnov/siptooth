//go:build linux

package hooks

import (
	"bufio"
	"os"
	"strings"
)

// hasAmbientCaps reports whether this process carries ambient capabilities (CapAmb != 0).
func hasAmbientCaps() bool {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "CapAmb:") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "CapAmb:"))
			return strings.Trim(v, "0") != ""
		}
	}
	return false
}
