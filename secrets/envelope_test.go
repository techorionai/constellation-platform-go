package secrets_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/secrets"
)

// testKey returns a base64 test key. The 32 bytes are all equal to seed. It is
// not a real key.
func testKey(seed byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

// testRing makes a ring. prev is a list of "id:seed" pairs.
func testRing(t testing.TB, id string, seed byte, prev ...struct {
	ID   string
	Seed byte
}) *secrets.KeyRing {
	t.Helper()
	var items []string
	for _, p := range prev {
		items = append(items, p.ID+":"+testKey(p.Seed))
	}
	ring, err := secrets.NewKeyRing(secrets.Config{
		MasterKey:    config.Secret(testKey(seed)),
		MasterKeyID:  id,
		PreviousKeys: config.Secret(strings.Join(items, ",")),
	})
	if err != nil {
		t.Fatal(err)
	}
	return ring
}

type prevKey = struct {
	ID   string
	Seed byte
}

// U1
func TestSealOpen(t *testing.T) {
	ring := testRing(t, "k1", 1)
	env, err := secrets.Seal(ring, []byte("value"), []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if env.Format != secrets.FormatV1 || env.KeyID != "k1" {
		t.Fatalf("envelope = format %d, key %q", env.Format, env.KeyID)
	}
	got, err := secrets.Open(ring, env, []byte("aad"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "value" {
		t.Fatalf("plaintext = %q", got)
	}
}

// U2
func TestSealIsRandom(t *testing.T) {
	ring := testRing(t, "k1", 1)
	a, _ := secrets.Seal(ring, []byte("value"), []byte("aad"))
	b, _ := secrets.Seal(ring, []byte("value"), []byte("aad"))
	if bytes.Equal(a.Nonce, b.Nonce) || bytes.Equal(a.WrappedDEK, b.WrappedDEK) || bytes.Equal(a.Ciphertext, b.Ciphertext) {
		t.Fatal("two envelopes share a nonce, a wrapped key, or a ciphertext")
	}
}

// U3
func TestOpenChangedCiphertext(t *testing.T) {
	ring := testRing(t, "k1", 1)
	env, _ := secrets.Seal(ring, []byte("value"), []byte("aad"))
	env.Ciphertext[0] ^= 1
	if _, err := secrets.Open(ring, env, []byte("aad")); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("err = %v, want ErrDecrypt", err)
	}
}

// U4
func TestOpenChangedWrappedDEK(t *testing.T) {
	ring := testRing(t, "k1", 1)
	env, _ := secrets.Seal(ring, []byte("value"), []byte("aad"))
	env.WrappedDEK[len(env.WrappedDEK)-1] ^= 1
	if _, err := secrets.Open(ring, env, []byte("aad")); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("err = %v, want ErrDecrypt", err)
	}
	// A wrong size gives ErrDecrypt too, with no panic.
	env.WrappedDEK = env.WrappedDEK[:5]
	if _, err := secrets.Open(ring, env, []byte("aad")); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("short wrapped key: err = %v, want ErrDecrypt", err)
	}
}

// U5
func TestOpenOtherAAD(t *testing.T) {
	ring := testRing(t, "k1", 1)
	env, _ := secrets.Seal(ring, []byte("value"), secrets.DataAAD("a"))
	if _, err := secrets.Open(ring, env, secrets.DataAAD("b")); !errors.Is(err, secrets.ErrDecrypt) {
		t.Fatalf("err = %v, want ErrDecrypt", err)
	}
}

// U6
func TestOpenUnknownKey(t *testing.T) {
	env, _ := secrets.Seal(testRing(t, "k1", 1), []byte("value"), nil)
	if _, err := secrets.Open(testRing(t, "k2", 2), env, nil); !errors.Is(err, secrets.ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
}

// U7
func TestNewKeyRingErrors(t *testing.T) {
	good := testKey(1)
	cases := map[string]secrets.Config{
		"short key":       {MasterKey: config.Secret(base64.StdEncoding.EncodeToString(make([]byte, 31))), MasterKeyID: "k1"},
		"not base64":      {MasterKey: "this is not base64 !!!", MasterKeyID: "k1"},
		"duplicate ids":   {MasterKey: config.Secret(good), MasterKeyID: "k1", PreviousKeys: config.Secret("k2:" + testKey(2) + ",k2:" + testKey(3))},
		"current in list": {MasterKey: config.Secret(good), MasterKeyID: "k1", PreviousKeys: config.Secret("k1:" + testKey(2))},
		"upper case id":   {MasterKey: config.Secret(good), MasterKeyID: "K1"},
		"long id":         {MasterKey: config.Secret(good), MasterKeyID: strings.Repeat("a", 33)},
		"no colon":        {MasterKey: config.Secret(good), MasterKeyID: "k1", PreviousKeys: config.Secret(testKey(2))},
		"bad previous":    {MasterKey: config.Secret(good), MasterKeyID: "k1", PreviousKeys: config.Secret("k2:AAAA")},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := secrets.NewKeyRing(cfg)
			if !errors.Is(err, secrets.ErrInvalidConfig) {
				t.Fatalf("err = %v, want ErrInvalidConfig", err)
			}
			for _, k := range []string{cfg.MasterKey.Reveal(), cfg.PreviousKeys.Reveal()} {
				if k != "" && strings.Contains(err.Error(), k) {
					t.Fatalf("the error contains key text: %v", err)
				}
			}
		})
	}
}

// U8
func TestKeyRingFromFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "master")
	if err := os.WriteFile(keyFile, []byte(testKey(7)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SVC_SECRETS__MASTER_KEY_FILE", keyFile)
	t.Setenv("SVC_SECRETS__MASTER_KEY_ID", "file-key")
	var cfg struct {
		Secrets secrets.Config `envPrefix:"SECRETS__"`
	}
	if err := config.Load("SVC_", &cfg); err != nil {
		t.Fatal(err)
	}
	ring, err := secrets.NewKeyRing(cfg.Secrets)
	if err != nil {
		t.Fatal(err)
	}
	if ring.CurrentKeyID() != "file-key" {
		t.Fatalf("current key ID = %q", ring.CurrentKeyID())
	}
	// The key from the file is the key of seed 7.
	env, _ := secrets.Seal(testRing(t, "file-key", 7), []byte("v"), nil)
	if _, err := secrets.Open(ring, env, nil); err != nil {
		t.Fatalf("the ring has another key than the file: %v", err)
	}
}

// U9
func TestOpenWithPreviousKey(t *testing.T) {
	env, _ := secrets.Seal(testRing(t, "k1", 1), []byte("value"), []byte("aad"))
	ring2 := testRing(t, "k2", 2, prevKey{"k1", 1})
	got, err := secrets.Open(ring2, env, []byte("aad"))
	if err != nil || string(got) != "value" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

// U10
func TestRewrapEnvelope(t *testing.T) {
	env, _ := secrets.Seal(testRing(t, "k1", 1), []byte("value"), []byte("aad"))
	ring2 := testRing(t, "k2", 2, prevKey{"k1", 1})
	out, err := secrets.Rewrap(ring2, env)
	if err != nil {
		t.Fatal(err)
	}
	if out.KeyID != "k2" || !bytes.Equal(out.Ciphertext, env.Ciphertext) || !bytes.Equal(out.Nonce, env.Nonce) {
		t.Fatal("rewrap changed the data or kept the old key")
	}
	got, err := secrets.Open(testRing(t, "k2", 2), out, []byte("aad"))
	if err != nil || string(got) != "value" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

// U32
func TestRotationBothDirections(t *testing.T) {
	a := testRing(t, "k1", 1, prevKey{"k2", 2})
	b := testRing(t, "k2", 2, prevKey{"k1", 1})
	for _, pair := range [][2]*secrets.KeyRing{{a, b}, {b, a}} {
		env, err := secrets.Seal(pair[0], []byte("value"), []byte("aad"))
		if err != nil {
			t.Fatal(err)
		}
		got, err := secrets.Open(pair[1], env, []byte("aad"))
		if err != nil || string(got) != "value" {
			t.Fatalf("got %q, err %v", got, err)
		}
	}
}

// U33
func TestOpenUnsupportedFormat(t *testing.T) {
	ring := testRing(t, "k1", 1)
	env, _ := secrets.Seal(ring, []byte("value"), nil)
	env.Format = 2
	if _, err := secrets.Open(ring, env, nil); !errors.Is(err, secrets.ErrUnsupportedFormat) {
		t.Fatalf("err = %v, want ErrUnsupportedFormat", err)
	}
}

// U39
func TestAADLabels(t *testing.T) {
	wrap, data := string(secrets.WrapAAD("k1")), string(secrets.DataAAD("k1"))
	if wrap != "constellation-secrets:v1:kek:k1" || data != "constellation-secrets:v1:data:k1" {
		t.Fatalf("wrap AAD %q, data AAD %q", wrap, data)
	}
	if wrap == data {
		t.Fatal("the two AAD values are equal")
	}
}
