package secrets

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/techorionai/constellation-platform-go/config"
)

// keySize is the size of a master key and of a data key, in bytes.
const keySize = 32

var keyIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ErrInvalidConfig means that [Config] is not valid. The text names the
// problem and never contains a key.
var ErrInvalidConfig = errors.New("secrets: invalid config")

// Config holds the master keys. A service nests it in its config struct with
// the tag envPrefix:"SECRETS__".
type Config struct {
	// MasterKey is the current master key: 32 bytes, base64 (standard
	// encoding). Variable: MASTER_KEY or MASTER_KEY_FILE.
	MasterKey config.Secret `env:"MASTER_KEY,required" envFile:"true"`
	// MasterKeyID is the ID of the current master key. It matches
	// ^[a-z0-9-]{1,32}$. Variable: MASTER_KEY_ID.
	MasterKeyID string `env:"MASTER_KEY_ID,required"`
	// PreviousKeys are the master keys for rotation. The format is
	// <id>:<base64> items, separated by commas. Variable: PREVIOUS_KEYS or
	// PREVIOUS_KEYS_FILE. It is optional.
	PreviousKeys config.Secret `env:"PREVIOUS_KEYS" envFile:"true"`
}

// KeyRing holds the current master key and the previous master keys.
type KeyRing struct {
	currentID string
	keys      map[string][]byte
}

// NewKeyRing checks cfg and makes a key ring. It checks that each key decodes to
// 32 bytes, that each key ID matches ^[a-z0-9-]{1,32}$, that the IDs are unique,
// and that the current ID is not in the list of previous keys. An error never
// contains a key.
func NewKeyRing(cfg Config) (*KeyRing, error) {
	if !keyIDPattern.MatchString(cfg.MasterKeyID) {
		return nil, fmt.Errorf("%w: the master key ID must match %s", ErrInvalidConfig, keyIDPattern)
	}
	current, err := decodeKey(cfg.MasterKey.Reveal())
	if err != nil {
		return nil, fmt.Errorf("%w: master key: %w", ErrInvalidConfig, err)
	}
	ring := &KeyRing{currentID: cfg.MasterKeyID, keys: map[string][]byte{cfg.MasterKeyID: current}}

	raw := strings.TrimSpace(cfg.PreviousKeys.Reveal())
	if raw == "" {
		return ring, nil
	}
	for i, item := range strings.Split(raw, ",") {
		id, encoded, ok := strings.Cut(strings.TrimSpace(item), ":")
		if !ok {
			return nil, fmt.Errorf("%w: previous key item %d must have the format <id>:<base64>", ErrInvalidConfig, i+1)
		}
		if !keyIDPattern.MatchString(id) {
			return nil, fmt.Errorf("%w: the ID of previous key item %d must match %s", ErrInvalidConfig, i+1, keyIDPattern)
		}
		if _, dup := ring.keys[id]; dup {
			return nil, fmt.Errorf("%w: key ID %q is used more than one time", ErrInvalidConfig, id)
		}
		key, err := decodeKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("%w: previous key %q: %w", ErrInvalidConfig, id, err)
		}
		ring.keys[id] = key
	}
	return ring, nil
}

// CurrentKeyID returns the ID of the current master key.
func (r *KeyRing) CurrentKeyID() string { return r.currentID }

func (r *KeyRing) lookup(id string) ([]byte, bool) {
	key, ok := r.keys[id]
	return key, ok
}

// decodeKey decodes a base64 key. The error text never contains the key.
func decodeKey(s string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("the key is not valid base64")
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("the key must have %d bytes", keySize)
	}
	return key, nil
}
