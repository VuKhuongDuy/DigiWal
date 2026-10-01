package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Whitelist Entry states. Only `pending` → `active` exists, and it is a function of time.
const (
	WhitelistPending = "pending"
	WhitelistActive  = "active"
)

type WhitelistEntry struct {
	ID      string
	UserID  string
	Address string // EIP-55
	AddedAt time.Time
}

// Status is the entry's state at now: active once cooldown has passed since AddedAt
// (invariant #13), computed on read.
func (e WhitelistEntry) Status(now time.Time, cooldown time.Duration) string {
	if !now.Before(e.AddedAt.Add(cooldown)) {
		return WhitelistActive
	}
	return WhitelistPending
}

// AddWhitelistEntry inserts (user_id, address), or returns the existing entry unchanged:
// re-adding never errors and never restarts the cooldown clock (invariant #15). address must
// already be in EIP-55 form, so two spellings of one address are one entry.
func (s *Store) AddWhitelistEntry(ctx context.Context, userID, address string) (e WhitelistEntry, created bool, err error) {
	err = s.pool.QueryRow(ctx, `
		INSERT INTO whitelist_entries (user_id, address) VALUES ($1, $2)
		ON CONFLICT (user_id, address) DO NOTHING
		RETURNING id, user_id, address, added_at`, userID, address).
		Scan(&e.ID, &e.UserID, &e.Address, &e.AddedAt)
	if err == nil {
		return e, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return WhitelistEntry{}, false, err
	}
	err = s.pool.QueryRow(ctx, `
		SELECT id, user_id, address, added_at FROM whitelist_entries WHERE user_id = $1 AND address = $2`,
		userID, address).Scan(&e.ID, &e.UserID, &e.Address, &e.AddedAt)
	return e, false, err
}

// ListWhitelist returns the user's own entries, newest first, so a user can spot an entry they
// did not add while it is still pending (the reason the cooldown exists).
func (s *Store) ListWhitelist(ctx context.Context, userID string) ([]WhitelistEntry, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, user_id, address, added_at FROM whitelist_entries
		WHERE user_id = $1 ORDER BY added_at DESC, id`, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (WhitelistEntry, error) {
		var e WhitelistEntry
		err := r.Scan(&e.ID, &e.UserID, &e.Address, &e.AddedAt)
		return e, err
	})
}

// RemoveWhitelistEntry deletes the user's own entry, effective immediately (invariant #13).
// Idempotent: an id that does not exist, or belongs to another user, is a no-op.
func (s *Store) RemoveWhitelistEntry(ctx context.Context, userID, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM whitelist_entries WHERE id = $1 AND user_id = $2`, id, userID)
	return err
}

// WhitelistStatus returns the state of the user's entry for address at now, or ok=false if
// there is none. Validation (task 3.2) accepts a withdrawal only when this is active (#12).
// The cooldown comparison runs against the database clock, so the app's clock cannot
// disagree with the stored added_at.
func (s *Store) WhitelistStatus(ctx context.Context, userID, address string, cooldown time.Duration) (status string, ok bool, err error) {
	var active bool
	err = s.pool.QueryRow(ctx, `
		SELECT now() >= added_at + make_interval(secs => $3)
		FROM whitelist_entries WHERE user_id = $1 AND address = $2`,
		userID, address, cooldown.Seconds()).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if active {
		return WhitelistActive, true, nil
	}
	return WhitelistPending, true, nil
}

// Now is the database clock, so a response's status agrees with WhitelistStatus.
func (s *Store) Now(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(ctx, `SELECT now()`).Scan(&t)
	return t, err
}
