package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
)

type Vault struct {
	aead cipher.AEAD
	key  []byte
}

func Open(dataDir string) (*Vault, error) {
	path := filepath.Join(dataDir, "master.key")
	key, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		file, openErr := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if openErr != nil {
			return nil, openErr
		}
		if _, err = file.Write(key); err != nil {
			file.Close()
			return nil, err
		}
		if err = file.Close(); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("master key must be 32 bytes")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead, key: append([]byte(nil), key...)}, nil
}

func (v *Vault) Seal(plain []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, v.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return v.aead.Seal(nil, nonce, plain, nil), nonce, nil
}

// SealDeterministic derives an AEAD nonce from the vault key and a caller
// context. It is intended only for immutable, content-addressed chunks where
// the context includes a plaintext digest and chunk index, so a context cannot
// be reused for different plaintext without a hash collision.
func (v *Vault) SealDeterministic(plain []byte, context []byte) (ciphertext, nonce []byte, err error) {
	if len(context) == 0 {
		return nil, nil, fmt.Errorf("deterministic encryption context is required")
	}
	mac := hmac.New(sha256.New, v.key)
	_, _ = mac.Write([]byte("axiom-deterministic-aead-nonce-v1\x00"))
	_, _ = mac.Write(context)
	nonce = append([]byte(nil), mac.Sum(nil)[:v.aead.NonceSize()]...)
	return v.aead.Seal(nil, nonce, plain, nil), nonce, nil
}

func (v *Vault) Open(ciphertext, nonce []byte) ([]byte, error) {
	return v.aead.Open(nil, nonce, ciphertext, nil)
}
