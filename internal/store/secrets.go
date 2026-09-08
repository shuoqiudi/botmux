package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
)

const secretEnvelopeVersion = "v1"

// LoadSecretKey reads an AES-256 key from a mounted file. Raw 32-byte, hex,
// and base64 encodings are accepted so operators can use common secret stores.
func LoadSecretKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) == 32 {
		return append([]byte(nil), raw...), nil
	}
	text := strings.TrimSpace(string(raw))
	if decoded, err := hex.DecodeString(text); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(text); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	return nil, errors.New("Gateway key file must contain exactly 32 raw bytes, 64 hex characters, or base64 for 32 bytes")
}

func loadOrCreateSecretKey(dbPath string) ([]byte, error) {
	if dbPath == ":memory:" || strings.HasPrefix(dbPath, "file::memory:") {
		key := make([]byte, 32)
		_, err := rand.Read(key)
		return key, err
	}
	path := dbPath + ".gateway-key"
	if key, err := LoadSecretKey(path); err == nil {
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0600); err != nil {
		return nil, fmt.Errorf("create Gateway key file: %w", err)
	}
	return key, nil
}

func (s *Store) sealSecret(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	block, err := aes.NewCipher(s.secretKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return secretEnvelopeVersion + ":" + base64.RawStdEncoding.EncodeToString(sealed), nil
}

func (s *Store) openSecret(envelope string) (string, error) {
	if envelope == "" {
		return "", nil
	}
	parts := strings.SplitN(envelope, ":", 2)
	if len(parts) != 2 || parts[0] != secretEnvelopeVersion {
		return "", errors.New("unsupported encrypted secret envelope")
	}
	sealed, err := base64.RawStdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("invalid encrypted secret envelope")
	}
	block, err := aes.NewCipher(s.secretKey)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("invalid encrypted secret envelope")
	}
	plain, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		return "", errors.New("Gateway credential could not be decrypted with the configured key")
	}
	return string(plain), nil
}

func (s *Store) secretFingerprint(secret string) string {
	mac := hmac.New(sha256.New, s.secretKey)
	_, _ = mac.Write([]byte(secret))
	return hex.EncodeToString(mac.Sum(nil))
}

func addColumnIfMissing(db *sql.DB, table, column, definition string) error {
	var count int
	if err := db.QueryRow(fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('%s') WHERE name=?", table), column).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		_, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
		return err
	}
	return nil
}
