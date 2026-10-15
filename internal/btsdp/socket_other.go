//go:build !linux

package btsdp

import (
	"context"
	"errors"
	"io"
	"os"
)

// ErrUnsupported is returned by the socket functions on platforms without Bluetooth sockets.
var ErrUnsupported = errors.New("btsdp: bluetooth sockets are only supported on linux")

func DialSDP(ctx context.Context, adapter, phone string) (Transport, io.Closer, error) {
	return nil, nil, ErrUnsupported
}

func DialRFCOMM(ctx context.Context, adapter, phone string, channel uint8) (*os.File, error) {
	return nil, ErrUnsupported
}

func FindSMSMAS(ctx context.Context, adapter, phone string) (MASRecord, error) {
	return MASRecord{}, ErrUnsupported
}
