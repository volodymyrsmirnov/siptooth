//go:build !linux

package sco

import "context"

func Listen(adapter string) (Listener, error) { return nil, ErrUnsupported }

type dialer struct{}

func NewDialer() Dialer { return dialer{} }

func (dialer) Dial(ctx context.Context, adapter, remote string) (Link, error) {
	return nil, ErrUnsupported
}
