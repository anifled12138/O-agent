package secure

import (
	"bytes"
	"testing"
)

func TestDeterministicSealIsStablePerContextAndAuthenticates(t *testing.T) {
	vault, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte("immutable chunk bytes")
	firstCipher, firstNonce, err := vault.SealDeterministic(plain, []byte("snapshot-sha256:chunk:0"))
	if err != nil {
		t.Fatal(err)
	}
	secondCipher, secondNonce, err := vault.SealDeterministic(plain, []byte("snapshot-sha256:chunk:0"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstCipher, secondCipher) || !bytes.Equal(firstNonce, secondNonce) {
		t.Fatal("same immutable plaintext and context must produce resumable ciphertext")
	}
	otherCipher, otherNonce, err := vault.SealDeterministic(plain, []byte("snapshot-sha256:chunk:1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(firstNonce, otherNonce) || bytes.Equal(firstCipher, otherCipher) {
		t.Fatal("different chunk contexts reused deterministic ciphertext")
	}
	opened, err := vault.Open(firstCipher, firstNonce)
	if err != nil || !bytes.Equal(opened, plain) {
		t.Fatalf("deterministic ciphertext did not decrypt: %q %v", opened, err)
	}
	if _, _, err := vault.SealDeterministic(plain, nil); err == nil {
		t.Fatal("deterministic seal accepted an empty nonce context")
	}
}
