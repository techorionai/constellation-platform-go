// Package secrets stores secret values with envelope encryption.
//
// [Seal] makes a random data key for each value, encrypts the value with
// AES-256-GCM and the data key, and wraps the data key with the master key.
// [Open] reverses this. A [KeyRing] holds the current master key and the
// previous master keys, so that a service can rotate the master key. The
// [Service] checks the names and the sizes, calls [Seal] and [Open], and stores
// the [Envelope] through the [Repository] port. The PostgreSQL adapter is in the
// package secrets/postgres. This package does not import a database driver.
//
// A service loads [Config] from the environment, makes a key ring, and makes a
// service:
//
//	var cfg struct {
//		Secrets secrets.Config `envPrefix:"SECRETS__"`
//	}
//	if err := config.Load("IDENTITY_", &cfg); err != nil {
//		return err
//	}
//	ring, err := secrets.NewKeyRing(cfg.Secrets)
//	if err != nil {
//		return err
//	}
//	svc := secrets.NewService(ring, repo)
//	err = svc.Put(ctx, "llm/api-key", value)
//
// No error, span, or log record of this package contains a secret value or a
// key. The caller checks the permissions and writes the audit entries.
package secrets
