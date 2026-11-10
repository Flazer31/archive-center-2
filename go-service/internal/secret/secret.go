// Package secret keeps the backend's secrets on disk: files only its own
// account can read (the access token, the key file) and the encryption of
// the API keys it stores, so the database never holds a key in the clear.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"archivecenter2/goservice/internal/osfile"
)

// WriteFile writes data to a temporary file of its own, restricts it to the
// owner, then puts it in place, so the file is never seen half written or
// open to others.
func WriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".secret-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if err := Restrict(tmp); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := osfile.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// Box seals and opens API keys with AES-256-GCM under the key in its key
// file.
type Box struct{ aead cipher.AEAD }

const sealPrefix = "v1:"

// LoadBox reads the key file, or makes it when there is none.
func LoadBox(path string) (*Box, error) {
	key, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(key) != 32 {
			return nil, errors.New("key file " + path + " is not 32 bytes")
		}
		if err := Restrict(path); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
		key = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, err
		}
		if err := WriteFile(path, key); err != nil {
			return nil, err
		}
	default:
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}

// Seal encrypts a key for storage.
func (b *Box) Seal(plain string) (string, error) {
	nonce := make([]byte, b.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	out := b.aead.Seal(nonce, nonce, []byte(plain), nil)
	return sealPrefix + base64.StdEncoding.EncodeToString(out), nil
}

// Open decrypts a stored key.
func (b *Box) Open(sealed string) (string, error) {
	if !strings.HasPrefix(sealed, sealPrefix) {
		return "", errors.New("stored key is not sealed")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(sealed, sealPrefix))
	if err != nil {
		return "", err
	}
	n := b.aead.NonceSize()
	if len(raw) < n {
		return "", errors.New("stored key is too short")
	}
	plain, err := b.aead.Open(nil, raw[:n], raw[n:], nil)
	if err != nil {
		return "", errors.New("stored key cannot be opened with this key file")
	}
	return string(plain), nil
}
