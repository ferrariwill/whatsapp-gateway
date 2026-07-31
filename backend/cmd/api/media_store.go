package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultMediaTTL = 15 * time.Minute

type mediaObject struct {
	ID        string
	SystemID  string
	TenantID  string
	MimeType  string
	Filename  string
	Data      []byte
	ExpiresAt time.Time
}

type mediaStore struct {
	mu      sync.RWMutex
	objects map[string]*mediaObject
	secret  []byte
}

func newMediaStore(secret []byte) *mediaStore {
	if len(secret) == 0 {
		secret = []byte("dev-media-signing-secret-change-me!!")
	}
	return &mediaStore{
		objects: make(map[string]*mediaObject),
		secret:  secret,
	}
}

func (s *mediaStore) Put(systemID, tenantID, mimeType, filename string, data []byte, ttl time.Duration) (*mediaObject, error) {
	if ttl <= 0 {
		ttl = defaultMediaTTL
	}
	id, err := randomMediaID()
	if err != nil {
		return nil, err
	}
	obj := &mediaObject{
		ID:        id,
		SystemID:  systemID,
		TenantID:  tenantID,
		MimeType:  mimeType,
		Filename:  filename,
		Data:      append([]byte(nil), data...),
		ExpiresAt: time.Now().UTC().Add(ttl),
	}
	s.mu.Lock()
	s.objects[id] = obj
	s.mu.Unlock()
	return obj, nil
}

func (s *mediaStore) Get(id string) (*mediaObject, bool) {
	s.mu.RLock()
	obj, ok := s.objects[id]
	s.mu.RUnlock()
	if !ok {
		return nil, false
	}
	if time.Now().UTC().After(obj.ExpiresAt) {
		s.mu.Lock()
		delete(s.objects, id)
		s.mu.Unlock()
		return nil, false
	}
	return obj, true
}

// SignURLQuery builds exp + sig query values scoped to system_id + object id.
func (s *mediaStore) SignURLQuery(systemID, objectID string, exp time.Time) (expUnix string, sig string) {
	expUnix = strconv.FormatInt(exp.UTC().Unix(), 10)
	sig = s.sign(systemID, objectID, expUnix)
	return expUnix, sig
}

func (s *mediaStore) VerifySignature(systemID, objectID, expUnix, sig string) bool {
	if systemID == "" || objectID == "" || expUnix == "" || sig == "" {
		return false
	}
	exp, err := strconv.ParseInt(expUnix, 10, 64)
	if err != nil {
		return false
	}
	if time.Now().UTC().Unix() > exp {
		return false
	}
	expected := s.sign(systemID, objectID, expUnix)
	return hmac.Equal([]byte(expected), []byte(sig))
}

func (s *mediaStore) sign(systemID, objectID, expUnix string) string {
	payload := systemID + "|" + objectID + "|" + expUnix
	mac := hmac.New(sha256.New, s.secret)
	_, _ = mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

func randomMediaID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate media id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func mediaSigningSecret() []byte {
	if v := strings.TrimSpace(os.Getenv("MEDIA_SIGNING_SECRET")); v != "" {
		return []byte(v)
	}
	if v := strings.TrimSpace(os.Getenv("META_APP_SECRET")); v != "" {
		return []byte(v)
	}
	if v := strings.TrimSpace(os.Getenv("JWT_SECRET")); v != "" {
		return []byte(v)
	}
	return []byte("dev-media-signing-secret-change-me!!")
}

func publicBaseURL() string {
	base := strings.TrimSpace(os.Getenv("PUBLIC_BASE_URL"))
	if base == "" {
		base = strings.TrimSpace(os.Getenv("GATEWAY_PUBLIC_URL"))
	}
	return strings.TrimRight(base, "/")
}
