package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Box struct{ aead cipher.AEAD }

func New(encodedKey string) (Box, error) {
	key, err := base64.StdEncoding.DecodeString(encodedKey)
	if err != nil || len(key) != 32 {
		return Box{}, fmt.Errorf("secret key must be a base64-encoded 32-byte value")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return Box{}, err
	}
	aead, err := cipher.NewGCM(block)
	return Box{aead: aead}, err
}

func Generate() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

func LoadOrCreate(path string) (string, error) {
	if contents, err := os.ReadFile(path); err == nil {
		key := strings.TrimSpace(string(contents))
		if _, err := New(key); err != nil {
			return "", fmt.Errorf("read local connector key: %w", err)
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	key, err := Generate()
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return LoadOrCreate(path)
		}
		return "", err
	}
	if _, err := file.WriteString(key + "\n"); err != nil {
		file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return key, nil
}

func (b Box) Ready() bool { return b.aead != nil }

func (b Box) Seal(plaintext string) (ciphertext, nonce []byte, err error) {
	if !b.Ready() {
		return nil, nil, fmt.Errorf("connector secret encryption is not configured")
	}
	nonce = make([]byte, b.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return b.aead.Seal(nil, nonce, []byte(plaintext), nil), nonce, nil
}

func (b Box) Open(ciphertext, nonce []byte) (string, error) {
	if !b.Ready() {
		return "", fmt.Errorf("connector secret encryption is not configured")
	}
	plaintext, err := b.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt connector secret: %w", err)
	}
	return string(plaintext), nil
}
