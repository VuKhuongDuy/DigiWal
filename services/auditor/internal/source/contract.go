package source

// Store names a foreign store the auditor reads, and the directory of the service that owns its schema.
type Store struct {
	Name          string // used in logs and error messages
	Database      string // Postgres database name in the dev environment
	MigrationsDir string // relative to services/auditor
	RolesFile     string // relative to services/auditor
}

var Stores = []Store{
	{"ledger", "ledger", "../ledger-service/migrations", "roles/ledger.sql"},
	{"wallet", "custody", "../wallet-service/migrations", "roles/wallet.sql"},
	{"withdrawal", "withdrawal", "../withdrawal-service/migrations", "roles/withdrawal.sql"},
	{"chain-watcher", "chain_watcher", "../chain-watcher/migrations", "roles/chain_watcher.sql"},
	{"api-gateway", "api_gateway", "../api-gateway/migrations", "roles/api_gateway.sql"},
}

// Contract is every table and column the auditor reads, per store. It is the single list that the
// read-only roles (roles/*.sql, column-level grants) and the schema contract test are checked against
// (contract_test.go): a service migration that renames or drops one of these breaks that test in the
// service's CI, and the role grants nothing beyond it (least privilege).
//
// A rule that needs another column adds it here AND to the store's roles file, in the same change.
// Deliberately absent: wallet_seeds and master_wallet (invariant 9), withdrawal-service's signed_raw_tx and
// raw_tx, outbox payloads, API key hashes. hot_wallet_signatures.signed_raw_tx IS read: a signed tx is not
// key material, and T2 needs the hash of the original tx, which wallet-service does not store separately.
var Contract = map[string]map[string][]string{
	"ledger": {
		"assets":              {"id", "asset_type", "contract_address", "symbol", "decimals"},
		"accounts":            {"id", "asset_id", "kind", "user_id", "total_balance", "locked_balance"},
		"ledger_transactions": {"id", "source_type", "idempotency_key", "chain_id", "tx_hash", "log_index", "created_at", "purpose"},
		"entries":             {"id", "ledger_transaction_id", "account_id", "amount", "corrects_entry_id", "created_at"},
		"locks":               {"id", "idempotency_key", "account_id", "amount", "status", "created_at", "resolved_at"},
		"flagged_deposits":    {"chain_id", "tx_hash", "log_index", "asset_id", "amount", "status"},
	},
	"wallet": {
		"wallet_addresses":        {"user_id", "address"},
		"hot_wallet_nonces":       {"chain_id", "next_nonce", "frozen_from_nonce"},
		"hot_wallet_signatures":   {"idempotency_key", "chain_id", "nonce", "caller_cn", "created_at", "signed_raw_tx"},
		"hot_wallet_replacements": {"idempotency_key", "replacement_seq", "nonce", "tx_hash", "max_fee_per_gas", "max_priority_fee_per_gas", "kind", "created_at"},
		"hot_wallet_voids":        {"idempotency_key", "caller_cn", "chain_id"},
	},
	"withdrawal": {
		"withdrawal_requests": {"withdrawal_id", "user_id", "asset_id", "amount", "to_address", "status", "reject_reason", "tx_hash",
			"created_at", "updated_at", "nonce", "chain_id", "broadcast_at", "confirmed_block", "settle_ledger_tx_id", "confirmed_at",
			"disputed_from", "disputed_at", "broadcast_head_seen", "sent_head", "gas_tx_hash", "gas_used", "gas_price", "gas_purpose", "gas_posted_at"},
		"withdrawal_txs":    {"withdrawal_id", "replacement_seq", "tx_hash", "created_at"},
		"withdrawal_outbox": {"withdrawal_id", "topic", "created_at", "published_at"},
		"nonce_gaps":        {"chain_id", "from_nonce", "tx_hash", "detected_at", "reported_at"},
	},
	"chain-watcher": {
		"observed_transfers": {"chain_id", "tx_hash", "log_index", "asset_id", "from_address", "to_address", "user_id", "amount",
			"block_number", "block_hash", "status", "detected_at", "confirmed_at"},
		"watermark":      {"chain_id", "block_number", "updated_at"},
		"scanned_blocks": {"chain_id", "block_number", "block_hash"},
	},
	"api-gateway": {
		"users": {"id", "created_at"},
	},
}
