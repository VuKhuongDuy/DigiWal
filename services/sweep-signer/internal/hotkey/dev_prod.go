//go:build prod

package hotkey

import (
	"context"
	"crypto/tls"
	"errors"
)

// NewWalletService does not exist in a production build: the Hot address comes from hsm-gateway only.
func NewWalletService(string, *tls.Config, func(context.Context) (uint64, error)) (Source, error) {
	return nil, errors.New("HOT_SOURCE=wallet-service is a dev-profile source and is not compiled into a prod build")
}
