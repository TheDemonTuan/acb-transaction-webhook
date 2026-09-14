package security

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const EnvelopeVersion = "v1"

var (
	ErrAuthenticationFailed = errors.New("decryption authentication failed")
	ErrUnsupportedEnvelope  = errors.New("unsupported envelope version")
	ErrUnknownKeyID         = errors.New("unknown key id")
	ErrInvalidNonce         = errors.New("invalid nonce")
	ErrInvalidCiphertext    = errors.New("invalid ciphertext")
)

type Keyring struct {
	currentID string
	keys      map[string][]byte
}
type Envelope struct{ Version, KeyID, Nonce, Ciphertext string }

func NewKeyring(key []byte) (*Keyring, error) {
	if len(key) != 32 {
		return nil, errors.New("master key must be 32 bytes")
	}
	k := make([]byte, 32)
	copy(k, key)
	return &Keyring{currentID: "k1", keys: map[string][]byte{"k1": k}}, nil
}

func LoadKeyring(path string) (*Keyring, error) {
	if path == "" {
		return nil, errors.New("master key file is not configured")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	str := strings.TrimSpace(string(b))
	var key []byte
	if len(str) == 64 {
		if k, err := hex.DecodeString(str); err == nil && len(k) == 32 {
			key = k
		}
	}
	if len(key) == 0 {
		if k, err := base64.RawStdEncoding.DecodeString(str); err == nil && len(k) == 32 {
			key = k
		} else if k, err := base64.StdEncoding.DecodeString(str); err == nil && len(k) == 32 {
			key = k
		} else if len(b) == 32 {
			key = b
		}
	}
	if len(key) != 32 {
		return nil, errors.New("master key must be 32 bytes (raw, 64-char hex, or base64)")
	}
	return NewKeyring(key)
}

func (k *Keyring) Encrypt(plaintext, aad []byte) (Envelope, error) {
	key := k.keys[k.currentID]
	block, err := aes.NewCipher(key)
	if err != nil {
		return Envelope{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return Envelope{}, err
	}
	return Envelope{Version: EnvelopeVersion, KeyID: k.currentID, Nonce: base64.RawStdEncoding.EncodeToString(nonce), Ciphertext: base64.RawStdEncoding.EncodeToString(gcm.Seal(nil, nonce, plaintext, aad))}, nil
}

func (k *Keyring) Decrypt(e Envelope, aad []byte) ([]byte, error) {
	if e.Version != EnvelopeVersion {
		return nil, ErrUnsupportedEnvelope
	}
	key, ok := k.keys[e.KeyID]
	if !ok {
		return nil, ErrUnknownKeyID
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce, err := base64.RawStdEncoding.DecodeString(e.Nonce)
	if err != nil {
		return nil, ErrInvalidNonce
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(e.Ciphertext)
	if err != nil {
		return nil, ErrInvalidCiphertext
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrAuthenticationFailed, err)
	}
	return plaintext, nil
}

func VerifyHMAC(expected, actual []byte) bool {
	return subtle.ConstantTimeCompare(expected, actual) == 1
}
