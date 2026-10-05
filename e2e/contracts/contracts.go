// Package contracts embeds the compiled test contracts so tools outside the tests can deploy them.
package contracts

import _ "embed"

// MockERC20Bin is the creation bytecode of MockERC20.sol (constructor takes the initial supply).
//
//go:embed MockERC20.bin
var MockERC20Bin string
