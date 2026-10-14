//go:build !linux

package daemon

import (
	"context"
	"log/slog"

	"github.com/volodymyrsmirnov/siptooth/internal/config"
)

// runBluetooth has no telephony off Linux: the coordinator never gets a phone and never
// answers, while the control socket keeps working.
func runBluetooth(ctx context.Context, cfg config.Config, coord coordinator, d errorSink, logger *slog.Logger) {
	logger.Warn("bluetooth: not supported on this platform; running without telephony (control socket only)")
	<-ctx.Done()
}
