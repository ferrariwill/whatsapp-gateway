package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/whatsappgetway/gateway/internal/model"
)

const defaultMediaTTL = 24 * time.Hour

func mediaStorageDir() string {
	dir := strings.TrimSpace(os.Getenv("MEDIA_STORAGE_DIR"))
	if dir == "" {
		dir = filepath.Join(".", "data", "media")
	}
	return dir
}

func newMediaObjectID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// persistOutboundMedia stores bytes on disk under MEDIA_STORAGE_DIR using an
// internal UUID path (never client-supplied names) and inserts media_objects.
func (s *server) persistOutboundMedia(
	ctx context.Context,
	systemID, tenantID, mimeType, originalName string,
	data []byte,
	ttl time.Duration,
) (*model.MediaObject, error) {
	if ttl <= 0 {
		ttl = defaultMediaTTL
	}
	id, err := newMediaObjectID()
	if err != nil {
		return nil, fmt.Errorf("generate media id: %w", err)
	}
	sum := sha256.Sum256(data)
	rel := filepath.ToSlash(filepath.Join(systemID, id))
	absDir := filepath.Join(mediaStorageDir(), systemID)
	if err := os.MkdirAll(absDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir media storage: %w", err)
	}
	absPath := filepath.Join(absDir, id)
	if err := os.WriteFile(absPath, data, 0o640); err != nil {
		return nil, fmt.Errorf("write media file: %w", err)
	}

	expires := time.Now().UTC().Add(ttl)
	obj := &model.MediaObject{
		ID:           id,
		SystemID:     systemID,
		TenantID:     tenantID,
		SHA256:       hex.EncodeToString(sum[:]),
		MimeType:     mimeType,
		ByteSize:     int64(len(data)),
		StoragePath:  rel,
		OriginalName: originalName,
		ExpiresAt:    &expires,
	}
	if err := s.repo.CreateMediaObject(ctx, obj); err != nil {
		_ = os.Remove(absPath)
		return nil, err
	}
	return obj, nil
}

func absoluteMediaPath(storagePath string) string {
	cleaned := filepath.Clean(filepath.FromSlash(storagePath))
	return filepath.Join(mediaStorageDir(), cleaned)
}

// sweepExpiredMedia deletes expired DB rows and best-effort removes files.
func (s *server) sweepExpiredMedia(ctx context.Context) {
	objs, err := s.repo.DeleteExpiredMediaObjects(ctx, time.Now().UTC(), 200)
	if err != nil {
		log.Printf("media GC: %v", err)
		return
	}
	for _, obj := range objs {
		path := absoluteMediaPath(obj.StoragePath)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			log.Printf("media GC remove %s: %v", obj.ID, err)
		}
	}
	if len(objs) > 0 {
		log.Printf("media GC: removed %d expired object(s)", len(objs))
	}
}
