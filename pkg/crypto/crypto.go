// Package crypto provides AES encryption/decryption for refresh tokens at rest.
// The encryption key is stored in the project data directory.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	// keyFileName is the name of the file storing the encryption key.
	keyFileName = "encryption.key"
	// keyDir is the directory where the encryption key is stored.
	keyDir = "data/tokens"
)

// TokenRefreshError is returned when token refresh operations fail.
var (
	ErrKeyGeneration   = errors.New("failed to generate encryption key")
	ErrEncryption      = errors.New("encryption failed")
	ErrDecryption      = errors.New("decryption failed")
	ErrInvalidKey      = errors.New("invalid key length")
	ErrInvalidCiphertext = errors.New("invalid ciphertext")
)

// getKeyPath returns the full path to the encryption key file.
func getKeyPath() (string, error) {
	return filepath.Join(keyDir, keyFileName), nil
}

// loadOrCreateKey loads an existing encryption key or creates a new one.
// The key is stored in data/tokens/encryption.key with restricted permissions.
func loadOrCreateKey() ([]byte, error) {
	keyPath, err := getKeyPath()
	if err != nil {
		return nil, err
	}

	// Try to load existing key
	if keyData, err := os.ReadFile(keyPath); err == nil {
		return keyData, nil
	}

	// Create new key
	keyDir := filepath.Dir(keyPath)
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create key directory: %w", err)
	}

	key := make([]byte, 32) // AES-256
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrKeyGeneration, err)
	}

	if err := os.WriteFile(keyPath, key, 0600); err != nil {
		return nil, fmt.Errorf("failed to write key file: %w", err)
	}

	return key, nil
}

// Encrypt encrypts plaintext using AES-GCM.
// Returns base64-encoded ciphertext.
func Encrypt(plaintext string) (string, error) {
	key, err := loadOrCreateKey()
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrEncryption, err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("%w: failed to generate nonce", ErrEncryption)
	}

	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt decrypts base64-encoded ciphertext using AES-GCM.
// Returns the original plaintext.
func Decrypt(ciphertext string) (string, error) {
	key, err := loadOrCreateKey()
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidKey, err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDecryption, err)
	}

	data, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("%w: invalid base64", ErrInvalidCiphertext)
	}

	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("%w: ciphertext too short", ErrInvalidCiphertext)
	}

	nonce, ciphertextBytes := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertextBytes, nil)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrDecryption, err)
	}

	return string(plaintext), nil
}
