//go:build !softhsm

package gateway

import "digiwal/services/hsm-gateway/internal/hsm"

func openRealToken() hsm.Backend { panic("built without -tags softhsm") }

func openHelperMain() { panic("built without -tags softhsm") }
