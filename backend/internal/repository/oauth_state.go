package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	// ErrOAuthStateNotFound indica nonce inexistente, já consumido, expirado
	// ou com system_id/tenant_id divergente do callback.
	ErrOAuthStateNotFound = errors.New("oauth state nonce not found or already consumed")
	// ErrDuplicateOAuthStateNonce indica tentativa de registrar o mesmo hash duas vezes.
	ErrDuplicateOAuthStateNonce = errors.New("duplicate oauth state nonce")
)

// RegisterOAuthStateNonce persiste o hash SHA-256 do nonce (ainda não consumido).
func (r *PostgresRepository) RegisterOAuthStateNonce(
	ctx context.Context,
	systemID, tenantID, nonceHash string,
	expiresAt time.Time,
) error {
	const query = `
		INSERT INTO oauth_state_nonces (nonce_hash, system_id, tenant_id, expires_at)
		VALUES ($1, $2, $3, $4)
	`
	_, err := r.db.ExecContext(ctx, query, nonceHash, systemID, tenantID, expiresAt.UTC())
	if err != nil {
		if isUniqueViolation(err) {
			return ErrDuplicateOAuthStateNonce
		}
		return fmt.Errorf("register oauth state nonce: %w", err)
	}
	return nil
}

// ConsumeOAuthStateNonce marca o nonce como usado de forma atômica.
// Exige: consumed_at IS NULL, expires_at > NOW(), system_id e tenant_id exatos.
// Duas corridas concorrentes: só uma linha é atualizada (a segunda recebe ErrOAuthStateNotFound).
func (r *PostgresRepository) ConsumeOAuthStateNonce(
	ctx context.Context,
	nonceHash, systemID, tenantID string,
) error {
	const query = `
		UPDATE oauth_state_nonces
		SET consumed_at = NOW()
		WHERE nonce_hash = $1
		  AND system_id = $2
		  AND tenant_id = $3
		  AND consumed_at IS NULL
		  AND expires_at > NOW()
		RETURNING id
	`
	var id string
	err := r.db.QueryRowContext(ctx, query, nonceHash, systemID, tenantID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOAuthStateNotFound
	}
	if err != nil {
		return fmt.Errorf("consume oauth state nonce: %w", err)
	}
	return nil
}

// DeleteExpiredOAuthStateNonces remove nonces expirados (consumidos ou não) para GC.
func (r *PostgresRepository) DeleteExpiredOAuthStateNonces(ctx context.Context, olderThan time.Time) (int64, error) {
	const query = `
		DELETE FROM oauth_state_nonces
		WHERE expires_at < $1
	`
	result, err := r.db.ExecContext(ctx, query, olderThan.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete expired oauth state nonces: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete expired oauth state nonces rows affected: %w", err)
	}
	return n, nil
}
