package config

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// Password hashing for the management interface. PBKDF2-HMAC-SHA256 keeps the
// implementation dependency-free (crypto/pbkdf2 is in the standard library
// since Go 1.24) while still being slow enough to make offline guessing
// expensive. The encoded form is
//
//	pbkdf2-sha256$<iterations>$<base64 salt>$<base64 key>
const (
	pbkdf2Iterations = 210000
	pbkdf2SaltLength = 16
	pbkdf2KeyLength  = 32
	passwordMinLen   = 8
)

// HashPassword derives a storable hash for password with a random salt.
// Passwords shorter than eight bytes are rejected.
func HashPassword(password string) (string, error) {
	if len(password) < passwordMinLen {
		return "", fmt.Errorf("password must contain at least %d characters", passwordMinLen)
	}
	salt := make([]byte, pbkdf2SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, pbkdf2KeyLength)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

// ValidPasswordHash reports whether encoded looks like a hash this package
// produced, with parameters inside fixed bounds so a crafted value cannot
// demand excessive memory or CPU.
func ValidPasswordHash(encoded string) bool {
	_, _, _, ok := parsePasswordHash(encoded)
	return ok
}

// VerifyPassword reports whether password matches an encoded hash. The final
// comparison is constant time; a malformed hash never matches.
func VerifyPassword(encoded, password string) bool {
	iterations, salt, expected, ok := parsePasswordHash(encoded)
	if !ok {
		return false
	}
	actual, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

// VerifyPlain compares a plaintext password from config.json in constant time.
func VerifyPlain(configured, password string) bool {
	if configured == "" {
		return false
	}
	left := sha256.Sum256([]byte(configured))
	right := sha256.Sum256([]byte(password))
	return subtle.ConstantTimeCompare(left[:], right[:]) == 1
}

// parsePasswordHash decodes the stored form and bounds its parameters.
func parsePasswordHash(encoded string) (iterations int, salt, key []byte, ok bool) {
	parts := strings.Split(strings.TrimSpace(encoded), "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return 0, nil, nil, false
	}
	iterations, err := strconv.Atoi(parts[1])
	if err != nil || iterations < 10000 || iterations > 1000000 {
		return 0, nil, nil, false
	}
	salt, err = base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil || len(salt) < 8 || len(salt) > 64 {
		return 0, nil, nil, false
	}
	key, err = base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(key) < 16 || len(key) > 64 {
		return 0, nil, nil, false
	}
	return iterations, salt, key, true
}

// Check reports whether password matches this credential, preferring the hash.
func (w WebUI) Check(password string) bool {
	if hash := strings.TrimSpace(w.PasswordHash); hash != "" {
		return VerifyPassword(hash, password)
	}
	return VerifyPlain(w.Password, password)
}

// Fingerprint identifies the configured credential so sessions created under
// an old password stop working after a change.
func (w WebUI) Fingerprint() string {
	secret := strings.TrimSpace(w.PasswordHash)
	if secret == "" {
		secret = "plain:" + w.Password
	}
	sum := sha256.Sum256([]byte(secret))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// Configured reports whether the interface is enabled with a credential.
func (w WebUI) Configured() bool {
	return w.Enabled && (strings.TrimSpace(w.PasswordHash) != "" || strings.TrimSpace(w.Password) != "")
}

// WebUIPasswordFingerprint identifies the configured credential so sessions
// created under an old password stop working after a change.
func (c *Config) WebUIPasswordFingerprint() string {
	if c == nil || c.WebUI == nil {
		return ""
	}
	return c.WebUI.Fingerprint()
}

// CheckWebUIPassword verifies password against the configured hash or
// plaintext password, preferring the hash.
func (c *Config) CheckWebUIPassword(password string) bool {
	if c == nil || c.WebUI == nil {
		return false
	}
	return c.WebUI.Check(password)
}

// WebUIReady reports whether the management interface can be served: it must
// be enabled and have a credential configured.
func (c *Config) WebUIReady() bool {
	if c == nil || c.WebUI == nil {
		return false
	}
	return c.WebUI.Configured()
}
