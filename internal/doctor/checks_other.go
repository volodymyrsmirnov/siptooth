//go:build !linux

package doctor

import (
	"context"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

// platformChecks reports the Linux-only checks as skipped on other systems.
func platformChecks(ctx context.Context, cfg config.Config, opts Options) []Check {
	var out []Check
	for _, name := range []string{"bluetooth_group", "bluetoothd",
		"adapter", "adapter_powered", "phone_bonded", "sco_routing", "competing_handlers", "service_unit", "linger"} {
		out = append(out, skip(name, "linux only"))
	}
	return out
}
