package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
)

var ErrMediaObjectNotFound = errors.New("media object not found")

func (r *PostgresRepository) CreateMediaObject(ctx context.Context, obj *model.MediaObject) error {
	const q = `
		INSERT INTO media_objects (
			id, system_id, tenant_id, sha256, mime_type, byte_size, storage_path, original_name, expires_at
		) VALUES (
			COALESCE(NULLIF($1, '')::uuid, gen_random_uuid()),
			$2, $3, $4, $5, $6, $7, $8, $9
		)
		RETURNING id, created_at
	`
	idArg := obj.ID
	return r.db.QueryRowContext(
		ctx, q,
		idArg, obj.SystemID, obj.TenantID, obj.SHA256, obj.MimeType, obj.ByteSize,
		obj.StoragePath, nullIfEmpty(obj.OriginalName), obj.ExpiresAt,
	).Scan(&obj.ID, &obj.CreatedAt)
}

func (r *PostgresRepository) FindMediaObject(
	ctx context.Context,
	id, systemID, tenantID string,
) (*model.MediaObject, error) {
	const q = `
		SELECT id, system_id, tenant_id, sha256, mime_type, byte_size, storage_path,
		       COALESCE(original_name, ''), created_at, expires_at
		FROM media_objects
		WHERE id = $1 AND system_id = $2 AND tenant_id = $3
	`
	var obj model.MediaObject
	var expires sql.NullTime
	err := r.db.QueryRowContext(ctx, q, id, systemID, tenantID).Scan(
		&obj.ID, &obj.SystemID, &obj.TenantID, &obj.SHA256, &obj.MimeType, &obj.ByteSize,
		&obj.StoragePath, &obj.OriginalName, &obj.CreatedAt, &expires,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrMediaObjectNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find media object: %w", err)
	}
	if expires.Valid {
		t := expires.Time.UTC()
		obj.ExpiresAt = &t
		if time.Now().UTC().After(t) {
			return nil, ErrMediaObjectNotFound
		}
	}
	return &obj, nil
}

func (r *PostgresRepository) DeleteExpiredMediaObjects(ctx context.Context, now time.Time, limit int) ([]model.MediaObject, error) {
	if limit <= 0 {
		limit = 100
	}
	const q = `
		DELETE FROM media_objects
		WHERE id IN (
			SELECT id FROM media_objects
			WHERE expires_at IS NOT NULL AND expires_at < $1
			ORDER BY expires_at
			LIMIT $2
		)
		RETURNING id, system_id, tenant_id, sha256, mime_type, byte_size, storage_path,
		          COALESCE(original_name, ''), created_at, expires_at
	`
	rows, err := r.db.QueryContext(ctx, q, now.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("delete expired media: %w", err)
	}
	defer rows.Close()

	var out []model.MediaObject
	for rows.Next() {
		var obj model.MediaObject
		var expires sql.NullTime
		if err := rows.Scan(
			&obj.ID, &obj.SystemID, &obj.TenantID, &obj.SHA256, &obj.MimeType, &obj.ByteSize,
			&obj.StoragePath, &obj.OriginalName, &obj.CreatedAt, &expires,
		); err != nil {
			return nil, err
		}
		if expires.Valid {
			t := expires.Time.UTC()
			obj.ExpiresAt = &t
		}
		out = append(out, obj)
	}
	return out, rows.Err()
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
