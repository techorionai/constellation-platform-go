package config

import (
	"fmt"
	"log/slog"
)

const redacted = "[redacted]"

// Secret is a string type for secret values, for example a password or a
// connection URL with a password. All text, JSON, and log output of a Secret
// is "[redacted]". Use [Secret.Reveal] to get the value.
type Secret string

// Reveal returns the secret value.
func (s Secret) Reveal() string { return string(s) }

// String returns "[redacted]".
func (Secret) String() string { return redacted }

// GoString returns "[redacted]".
func (Secret) GoString() string { return redacted }

// Format writes "[redacted]" for all formatting verbs.
func (Secret) Format(f fmt.State, _ rune) {
	_, _ = f.Write([]byte(redacted))
}

// MarshalJSON returns the JSON string "[redacted]".
func (Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + redacted + `"`), nil
}

// MarshalText returns "[redacted]".
func (Secret) MarshalText() ([]byte, error) {
	return []byte(redacted), nil
}

// LogValue implements [slog.LogValuer]. It returns "[redacted]".
func (Secret) LogValue() slog.Value {
	return slog.StringValue(redacted)
}

var (
	_ fmt.Stringer   = Secret("")
	_ fmt.GoStringer = Secret("")
	_ fmt.Formatter  = Secret("")
	_ slog.LogValuer = Secret("")
)
