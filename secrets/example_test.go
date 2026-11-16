package secrets_test

import (
	"context"
	"encoding/base64"
	"fmt"

	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
	"github.com/techorionai/constellation-platform-go/secrets"
)

// exampleRing makes a ring with a key of 32 zero bytes. A service gets its
// master key from the environment. Never use a fixed key in production.
func exampleRing() *secrets.KeyRing {
	ring, err := secrets.NewKeyRing(secrets.Config{
		MasterKey:   config.Secret(base64.StdEncoding.EncodeToString(make([]byte, 32))),
		MasterKeyID: "k1",
	})
	if err != nil {
		panic(err)
	}
	return ring
}

func ExampleNewKeyRing() {
	_, err := secrets.NewKeyRing(secrets.Config{MasterKey: "not a key", MasterKeyID: "k1"})
	fmt.Println(err)
	// Output: secrets: invalid config: master key: the key is not valid base64
}

func ExampleSeal() {
	ring := exampleRing()
	aad := secrets.DataAAD("llm/api-key")
	env, err := secrets.Seal(ring, []byte("value"), aad)
	if err != nil {
		fmt.Println(err)
		return
	}
	plain, err := secrets.Open(ring, env, aad)
	fmt.Println(string(plain), err, env.KeyID)
	// Output: value <nil> k1
}

func ExampleService() {
	ctx := context.Background()
	// A service uses the PostgreSQL repository of secrets/postgres. The mock
	// keeps the records in memory.
	svc := secrets.NewService(exampleRing(), &mock.SecretsRepository{})

	if err := svc.Put(ctx, "llm/api-key", []byte("value")); err != nil {
		fmt.Println(err)
		return
	}
	value, err := svc.Get(ctx, "llm/api-key")
	fmt.Println(string(value), err)

	names, err := svc.List(ctx, secrets.ListParams{})
	fmt.Println(names.Names, err)

	_, err = svc.Get(ctx, "missing")
	fmt.Println(err)
	// Output:
	// value <nil>
	// [llm/api-key] <nil>
	// secrets: not found
}

func ExampleService_Rewrap() {
	ctx := context.Background()
	svc := secrets.NewService(exampleRing(), &mock.SecretsRepository{})
	if err := svc.Put(ctx, "a", []byte("value")); err != nil {
		fmt.Println(err)
		return
	}
	// All records use the current key, so there is nothing to rewrap.
	res, err := svc.Rewrap(ctx, 100)
	fmt.Println(res.Rewrapped, res.Conflicts, res.Failed, err)
	// Output: 0 0 0 <nil>
}
