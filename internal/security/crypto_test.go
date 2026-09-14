package security

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyringEncryptDecryptBindsAAD(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	path := filepath.Join(dir, "master.key")
	if err := os.WriteFile(path, []byte(base64.RawStdEncoding.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
	keyring, err := LoadKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := keyring.Encrypt([]byte("sensitive state"), []byte("session:connection-1:generation-2"))
	if err != nil {
		t.Fatal(err)
	}
	plaintext, err := keyring.Decrypt(envelope, []byte("session:connection-1:generation-2"))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(plaintext); got != "sensitive state" {
		t.Fatalf("plaintext = %q", got)
	}
	if _, err := keyring.Decrypt(envelope, []byte("session:connection-1:generation-3")); err == nil {
		t.Fatal("decrypt succeeded with different AAD")
	} else if !errors.Is(err, ErrAuthenticationFailed) {
		t.Fatalf("expected ErrAuthenticationFailed on mismatched AAD, got: %v", err)
	}
}

func TestLoadKeyringRejectsWrongSize(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "master.key")
	if err := os.WriteFile(path, []byte(base64.RawStdEncoding.EncodeToString(make([]byte, 31))), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKeyring(path); err == nil {
		t.Fatal("expected wrong-size key failure")
	}
}

func TestNewKeyringValidation(t *testing.T) {
	t.Parallel()
	if _, err := NewKeyring(make([]byte, 31)); err == nil {
		t.Fatal("expected error for 31-byte key")
	}
	if _, err := NewKeyring(make([]byte, 33)); err == nil {
		t.Fatal("expected error for 33-byte key")
	}
	kr, err := NewKeyring(make([]byte, 32))
	if err != nil || kr == nil {
		t.Fatalf("expected success for 32-byte key, got %v", err)
	}
}

func TestDecryptWithWrongKeyReturnsAuthenticationFailed(t *testing.T) {
	t.Parallel()
	keyA := bytes.Repeat([]byte{0xAA}, 32)
	keyB := bytes.Repeat([]byte{0xBB}, 32)

	krA, err := NewKeyring(keyA)
	if err != nil {
		t.Fatal(err)
	}
	krB, err := NewKeyring(keyB)
	if err != nil {
		t.Fatal(err)
	}

	secretData := []byte("top_secret_bark_device_key_xyz_999")
	aad := []byte("notification-secret:BARK:ch_123:k1")

	env, err := krA.Encrypt(secretData, aad)
	if err != nil {
		t.Fatal(err)
	}

	// Decrypt with original key succeeds
	decrypted, err := krA.Decrypt(env, aad)
	if err != nil {
		t.Fatalf("decryption with original key failed: %v", err)
	}
	if !bytes.Equal(decrypted, secretData) {
		t.Fatalf("decrypted data mismatch")
	}

	// Decrypt with wrong key fails with ErrAuthenticationFailed
	_, err = krB.Decrypt(env, aad)
	if err == nil {
		t.Fatal("expected decryption failure with wrong key")
	}
	if !errors.Is(err, ErrAuthenticationFailed) {
		t.Fatalf("expected ErrAuthenticationFailed, got: %v", err)
	}

	// Ensure error string does not leak secret data
	if strings.Contains(err.Error(), string(secretData)) {
		t.Fatalf("error string leaked secret data: %s", err.Error())
	}
}

func TestDecryptTamperedCiphertextAndNonce(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x12}, 32)
	kr, err := NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}

	env, err := kr.Encrypt([]byte("payload"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}

	// Tamper ciphertext
	ctBytes, err := base64.RawStdEncoding.DecodeString(env.Ciphertext)
	if err != nil {
		t.Fatal(err)
	}
	ctBytes[0] ^= 0xFF
	tamperedEnv := env
	tamperedEnv.Ciphertext = base64.RawStdEncoding.EncodeToString(ctBytes)

	if _, err := kr.Decrypt(tamperedEnv, []byte("aad")); !errors.Is(err, ErrAuthenticationFailed) {
		t.Fatalf("expected ErrAuthenticationFailed on tampered ciphertext, got: %v", err)
	}

	// Tamper nonce
	nonceBytes, err := base64.RawStdEncoding.DecodeString(env.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	nonceBytes[0] ^= 0xFF
	tamperedNonceEnv := env
	tamperedNonceEnv.Nonce = base64.RawStdEncoding.EncodeToString(nonceBytes)

	if _, err := kr.Decrypt(tamperedNonceEnv, []byte("aad")); !errors.Is(err, ErrAuthenticationFailed) {
		t.Fatalf("expected ErrAuthenticationFailed on tampered nonce, got: %v", err)
	}
}

func TestDecryptEnvelopeValidation(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x34}, 32)
	kr, err := NewKeyring(key)
	if err != nil {
		t.Fatal(err)
	}

	env, err := kr.Encrypt([]byte("payload"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}

	// Unsupported version
	badVerEnv := env
	badVerEnv.Version = "v99"
	if _, err := kr.Decrypt(badVerEnv, []byte("aad")); !errors.Is(err, ErrUnsupportedEnvelope) {
		t.Fatalf("expected ErrUnsupportedEnvelope, got: %v", err)
	}

	// Unknown key ID
	unknownKeyEnv := env
	unknownKeyEnv.KeyID = "nonexistent"
	if _, err := kr.Decrypt(unknownKeyEnv, []byte("aad")); !errors.Is(err, ErrUnknownKeyID) {
		t.Fatalf("expected ErrUnknownKeyID, got: %v", err)
	}

	// Invalid nonce base64
	badNonceEnv := env
	badNonceEnv.Nonce = "invalid!base64@"
	if _, err := kr.Decrypt(badNonceEnv, []byte("aad")); !errors.Is(err, ErrInvalidNonce) {
		t.Fatalf("expected ErrInvalidNonce, got: %v", err)
	}

	// Invalid ciphertext base64
	badCtEnv := env
	badCtEnv.Ciphertext = "invalid!ciphertext@"
	if _, err := kr.Decrypt(badCtEnv, []byte("aad")); !errors.Is(err, ErrInvalidCiphertext) {
		t.Fatalf("expected ErrInvalidCiphertext, got: %v", err)
	}
}
