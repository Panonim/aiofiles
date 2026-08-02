package auth

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SessionStore persists issued sessions so a restart does not sign everyone
// out. Implementations must be safe for concurrent use.
type SessionStore interface {
	Save(token string, expiry time.Time) error
	Delete(token string) error
	DeleteExpired(now time.Time) error
	Live(now time.Time) (map[string]time.Time, error) // unexpired as of now

	// Rebind records the credential sessions are being issued under and drops
	// every stored session when it differs from the recorded one. A store that
	// has never been rebound counts as differing, so the first run after an
	// upgrade starts from a known binding.
	Rebind(fingerprint string) error
}

type SQLSessionStore struct{ db *sql.DB }

var _ SessionStore = (*SQLSessionStore)(nil)

func NewSQLSessionStore(db *sql.DB) *SQLSessionStore { return &SQLSessionStore{db: db} }

// Timestamps are Unix milliseconds, matching the jobs table.

func (s *SQLSessionStore) Save(token string, expiry time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (token, expires_at) VALUES (?, ?)
		 ON CONFLICT(token) DO UPDATE SET expires_at = excluded.expires_at`,
		token, expiry.UnixMilli())
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}

func (s *SQLSessionStore) Delete(token string) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, token); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (s *SQLSessionStore) DeleteExpired(now time.Time) error {
	if _, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now.UnixMilli()); err != nil {
		return fmt.Errorf("delete expired sessions: %w", err)
	}
	return nil
}

func (s *SQLSessionStore) Rebind(fingerprint string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("rebind sessions: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	var stored string
	switch err := tx.QueryRow(`SELECT fingerprint FROM session_credential WHERE id = 1`).Scan(&stored); {
	case errors.Is(err, sql.ErrNoRows): // never bound; stored stays empty
	case err != nil:
		return fmt.Errorf("rebind sessions: %w", err)
	}

	if stored != fingerprint {
		if _, err := tx.Exec(`DELETE FROM sessions`); err != nil {
			return fmt.Errorf("rebind sessions: %w", err)
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO session_credential (id, fingerprint) VALUES (1, ?)
		 ON CONFLICT(id) DO UPDATE SET fingerprint = excluded.fingerprint`,
		fingerprint); err != nil {
		return fmt.Errorf("rebind sessions: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rebind sessions: %w", err)
	}
	return nil
}

func (s *SQLSessionStore) Live(now time.Time) (map[string]time.Time, error) {
	rows, err := s.db.Query(
		`SELECT token, expires_at FROM sessions WHERE expires_at > ?`, now.UnixMilli())
	if err != nil {
		return nil, fmt.Errorf("load sessions: %w", err)
	}
	defer rows.Close()

	out := make(map[string]time.Time)
	for rows.Next() {
		var token string
		var expiresMillis int64
		if err := rows.Scan(&token, &expiresMillis); err != nil {
			return nil, fmt.Errorf("load sessions: %w", err)
		}
		out[token] = time.UnixMilli(expiresMillis).UTC()
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load sessions: %w", err)
	}
	return out, nil
}
