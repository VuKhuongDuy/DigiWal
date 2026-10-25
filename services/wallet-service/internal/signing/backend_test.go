//go:build !prod

package signing

import "digiwal/services/wallet-service/internal/hdwallet"

// The software backend satisfies Backend.
var _ Backend = (*hdwallet.Wallet)(nil)
