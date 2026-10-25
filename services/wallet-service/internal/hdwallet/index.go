package hdwallet

// HotWalletIndex is the BIP44 index of the Hot Wallet's single address inside
// the hot_wallet seed (wallet-tiering.md invariant #1). It is one address, not
// one per user. Deliberately in an untagged file: it is a public constant, not key
// code, and the signing package needs it in every build including `-tags prod`.
const HotWalletIndex = 0
