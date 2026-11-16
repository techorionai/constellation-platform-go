package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
)

// FormatV1 is the only envelope format: AES-256-GCM for the data and for the
// wrap of the data key.
const FormatV1 = 1

const (
	nonceSize   = 12
	wrappedSize = nonceSize + keySize + 16 // nonce, data key, GCM tag
)

// Errors of [Seal], [Open], and [Rewrap]. None contains data or a key.
var (
	// ErrUnsupportedFormat means that Envelope.Format is not supported.
	ErrUnsupportedFormat = errors.New("secrets: unsupported envelope format")
	// ErrUnknownKey means that the key ring has no key with the key ID of the
	// envelope.
	ErrUnknownKey = errors.New("secrets: unknown master key")
	// ErrDecrypt means that the authentication failed. The key, the AAD, or the
	// envelope is not correct.
	ErrDecrypt = errors.New("secrets: cannot decrypt")
)

// Envelope is an encrypted value with its wrapped data key.
type Envelope struct {
	// Format is the envelope format. It is [FormatV1].
	Format int
	// KeyID is the ID of the master key that wrapped the data key.
	KeyID string
	// WrappedDEK is the nonce followed by the encrypted data key.
	WrappedDEK []byte
	// Nonce is the nonce of the data encryption.
	Nonce []byte
	// Ciphertext is the encrypted value.
	Ciphertext []byte
}

// WrapAAD returns the AAD for the wrap of a data key with the master key keyID.
func WrapAAD(keyID string) []byte {
	return []byte("constellation-secrets:v1:kek:" + keyID)
}

// DataAAD returns the AAD for the data of the secret name. A ciphertext that is
// copied to a row with a different name does not decrypt.
func DataAAD(name string) []byte {
	return []byte("constellation-secrets:v1:data:" + name)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// Seal encrypts plaintext with a new random data key, and wraps the data key
// with the current master key of ring. aad is the additional authenticated data
// of the data encryption.
func Seal(ring *KeyRing, plaintext, aad []byte) (Envelope, error) {
	dek := make([]byte, keySize)
	defer clear(dek)
	if _, err := rand.Read(dek); err != nil {
		return Envelope{}, errors.New("secrets: cannot make a data key")
	}
	data, err := newGCM(dek)
	if err != nil {
		return Envelope{}, errors.New("secrets: cannot set up the cipher")
	}
	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, errors.New("secrets: cannot make a nonce")
	}
	ciphertext := data.Seal(nil, nonce, plaintext, aad)

	wrapped, err := wrap(ring, ring.currentID, dek)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{Format: FormatV1, KeyID: ring.currentID, WrappedDEK: wrapped, Nonce: nonce, Ciphertext: ciphertext}, nil
}

// wrap encrypts dek with the master key keyID. The result is the nonce followed
// by the ciphertext.
func wrap(ring *KeyRing, keyID string, dek []byte) ([]byte, error) {
	key, ok := ring.lookup(keyID)
	if !ok {
		return nil, ErrUnknownKey
	}
	kek, err := newGCM(key)
	if err != nil {
		return nil, errors.New("secrets: cannot set up the cipher")
	}
	nonce := make([]byte, nonceSize, wrappedSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, errors.New("secrets: cannot make a nonce")
	}
	return kek.Seal(nonce, nonce, dek, WrapAAD(keyID)), nil
}

// unwrap returns the data key of env. The caller sets the result to zero.
func unwrap(ring *KeyRing, env Envelope) ([]byte, error) {
	if env.Format != FormatV1 {
		return nil, ErrUnsupportedFormat
	}
	key, ok := ring.lookup(env.KeyID)
	if !ok {
		return nil, ErrUnknownKey
	}
	if len(env.WrappedDEK) != wrappedSize || len(env.Nonce) != nonceSize {
		return nil, ErrDecrypt
	}
	kek, err := newGCM(key)
	if err != nil {
		return nil, errors.New("secrets: cannot set up the cipher")
	}
	dek, err := kek.Open(nil, env.WrappedDEK[:nonceSize], env.WrappedDEK[nonceSize:], WrapAAD(env.KeyID))
	if err != nil {
		return nil, ErrDecrypt
	}
	return dek, nil
}

// Open decrypts env. aad must be equal to the AAD that [Seal] got. It returns
// [ErrUnsupportedFormat], [ErrUnknownKey], or [ErrDecrypt].
func Open(ring *KeyRing, env Envelope, aad []byte) ([]byte, error) {
	dek, err := unwrap(ring, env)
	if err != nil {
		return nil, err
	}
	defer clear(dek)
	data, err := newGCM(dek)
	if err != nil {
		return nil, errors.New("secrets: cannot set up the cipher")
	}
	plaintext, err := data.Open(nil, env.Nonce, env.Ciphertext, aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return plaintext, nil
}

// Rewrap unwraps the data key of env with its master key and wraps it again with
// the current master key of ring. The data ciphertext and its nonce do not
// change. It returns the same errors as [Open].
func Rewrap(ring *KeyRing, env Envelope) (Envelope, error) {
	dek, err := unwrap(ring, env)
	if err != nil {
		return Envelope{}, err
	}
	defer clear(dek)
	wrapped, err := wrap(ring, ring.currentID, dek)
	if err != nil {
		return Envelope{}, fmt.Errorf("secrets: rewrap: %w", err)
	}
	return Envelope{
		Format:     FormatV1,
		KeyID:      ring.currentID,
		WrappedDEK: wrapped,
		Nonce:      append([]byte(nil), env.Nonce...),
		Ciphertext: append([]byte(nil), env.Ciphertext...),
	}, nil
}
