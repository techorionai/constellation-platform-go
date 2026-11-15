package valkey

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"
	"github.com/valkey-io/valkey-go/valkeyotel"
	"go.opentelemetry.io/otel"

	"github.com/techorionai/constellation-platform-go/config"
)

// ErrInvalidKeyPrefix means that the key prefix does not match
// ^[a-z][a-z0-9_]{0,31}$.
var ErrInvalidKeyPrefix = errors.New("valkey: key prefix must match ^[a-z][a-z0-9_]{0,31}$")

const defaultDialTimeout = 5 * time.Second

var prefixPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// Config is the configuration of the Valkey client. A service nests it with
// envPrefix:"VALKEY__".
type Config struct {
	// Address is the host and port of the server. It is required.
	Address string `env:"ADDRESS,required"`
	// Username is the name of the ACL user. If it is empty, the client uses the
	// default user, and the key prefix is not enforced by the server.
	Username string `env:"USERNAME"`
	// Password is the password of the user.
	Password config.Secret `env:"PASSWORD" envFile:"true"`
	// TLS turns on TLS for the connection.
	TLS bool `env:"TLS" envDefault:"false"`
	// DialTimeout is the time limit for a new connection. The default is 5
	// seconds.
	DialTimeout time.Duration `env:"DIAL_TIMEOUT" envDefault:"5s"`
	// KeyPrefix is the key prefix of the service, for example "identity". It is
	// required.
	KeyPrefix string `env:"KEY_PREFIX,required"`
}

// Client is a Valkey client with tracing, connection metrics, and the key
// prefix of the service.
type Client struct {
	// Raw is the valkey-go client. Make each key with Keys.
	Raw valkeygo.Client
	// Keys makes the keys of the service.
	Keys Keyspace
}

// New creates the client and sends PING. If a step fails, New closes the
// client before it returns the error.
func New(ctx context.Context, cfg Config) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("valkey: %w", err)
	}
	keys, err := NewKeyspace(cfg.KeyPrefix)
	if err != nil {
		return nil, err
	}
	if cfg.Address == "" {
		return nil, errors.New("valkey: address is required")
	}
	if cfg.Password.Reveal() != "" && !cfg.TLS {
		slog.Warn("valkey: the client has a password and TLS is off, so the server can read the password in plain text",
			slog.String("address", cfg.Address))
	}
	timeout := cfg.DialTimeout
	if timeout == 0 {
		timeout = defaultDialTimeout
	}
	opt := valkeygo.ClientOption{
		InitAddress: []string{cfg.Address},
		Username:    cfg.Username,
		Password:    cfg.Password.Reveal(),
		Dialer:      net.Dialer{Timeout: timeout},
		DisableCache: true,
	}
	if cfg.TLS {
		host, _, splitErr := net.SplitHostPort(cfg.Address)
		if splitErr != nil {
			host = cfg.Address
		}
		opt.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	}
	raw, err := valkeyotel.NewClient(opt,
		valkeyotel.WithMeterProvider(otel.GetMeterProvider()),
		valkeyotel.WithTracerProvider(otel.GetTracerProvider()),
	)
	if err != nil {
		return nil, fmt.Errorf("valkey: cannot connect to %s: %w", cfg.Address, err)
	}
	if err := raw.Do(ctx, raw.B().Ping().Build()).Error(); err != nil {
		raw.Close()
		return nil, fmt.Errorf("valkey: PING failed: %w", err)
	}
	if cfg.Username == "" {
		slog.Warn("valkey: the client uses the default user, so the server does not enforce the key prefix",
			slog.String("key_prefix", keys.Prefix()))
	}
	return &Client{Raw: raw, Keys: keys}, nil
}

// Close closes the client.
func (c *Client) Close() {
	c.Raw.Close()
}

// Keyspace makes the keys and the SCAN patterns of one service. All keys have
// the key prefix of the service.
type Keyspace struct {
	prefix string
}

// NewKeyspace returns a Keyspace for the prefix. The prefix must match
// ^[a-z][a-z0-9_]{0,31}$.
func NewKeyspace(prefix string) (Keyspace, error) {
	if !prefixPattern.MatchString(prefix) {
		return Keyspace{}, ErrInvalidKeyPrefix
	}
	return Keyspace{prefix: prefix}, nil
}

// Prefix returns the key prefix.
func (k Keyspace) Prefix() string { return k.prefix }

// Key joins the prefix and the parts with ":". Key panics if there are no parts
// or if a part is empty. A part can contain ":", so a part from outside input
// must not contain it. The caller checks this before the call.
func (k Keyspace) Key(parts ...string) string {
	checkParts(parts)
	return k.prefix + ":" + strings.Join(parts, ":")
}

// Pattern returns a SCAN pattern for the keys that start with the prefix and
// the parts, for example "identity:session:*". It escapes the glob characters
// in each part, so a part matches only itself.
func (k Keyspace) Pattern(parts ...string) string {
	escaped := make([]string, len(parts))
	for i, p := range parts {
		if p == "" {
			panic("valkey: empty key part")
		}
		escaped[i] = globEscaper.Replace(p)
	}
	return k.prefix + ":" + strings.Join(append(escaped, "*"), ":")
}

var globEscaper = strings.NewReplacer(`*`, `\*`, `?`, `\?`, `[`, `\[`, `]`, `\]`, `\`, `\\`)

func checkParts(parts []string) {
	if len(parts) == 0 {
		panic("valkey: a key needs one or more parts")
	}
	for _, p := range parts {
		if p == "" {
			panic("valkey: empty key part")
		}
	}
}
