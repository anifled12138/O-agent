package secure

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
)

type Vault struct{ aead cipher.AEAD }

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
	return &Vault{aead: aead}, nil
}

func (v *Vault) Seal(plain []byte) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, v.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return v.aead.Seal(nil, nonce, plain, nil), nonce, nil
}

func (v *Vault) Open(ciphertext, nonce []byte) ([]byte, error) {
	return v.aead.Open(nil, nonce, ciphertext, nil)
}
