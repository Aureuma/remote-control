package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"time"
)

type IssuedToken struct {
	Value     string
	ExpiresAt time.Time
}

func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func NewTokenWithTTL(ttl time.Duration) (IssuedToken, error) {
	if ttl <= 0 {
		ttl = time.Hour
	}
	value, err := NewToken()
	if err != nil {
		return IssuedToken{}, err
	}
	return IssuedToken{
		Value:     value,
		ExpiresAt: time.Now().UTC().Add(ttl),
	}, nil
}

func IsExpired(expiresAt, now time.Time) bool {
	if expiresAt.IsZero() {
		return false
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	return !now.Before(expiresAt)
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
