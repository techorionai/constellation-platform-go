package postgres_test

import (
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/config"
)

func closedPort(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	return addr
}

func cfgFor(t *testing.T, path, database string) postgres.Config {
	t.Helper()
	return postgres.Config{
		URL:      config.Secret("postgres://user:s3cr3t@" + closedPort(t) + path),
		Database: database,
	}
}

// U19
func TestNewConnectErrorHidesPassword(t *testing.T) {
	_, err := postgres.New(t.Context(), cfgFor(t, "/a", "a"))
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("error has the password: %v", err)
	}
}

// U24
func TestNewDatabaseMismatch(t *testing.T) {
	_, err := postgres.New(t.Context(), cfgFor(t, "/b", "a"))
	if !errors.Is(err, postgres.ErrDatabaseMismatch) {
		t.Fatalf("got %v", err)
	}
}

// U25
func TestNewDatabaseNames(t *testing.T) {
	tests := []struct {
		name string
		want error
	}{
		{"postgres", postgres.ErrSystemDatabase},
		{"template0", postgres.ErrSystemDatabase},
		{"template1", postgres.ErrSystemDatabase},
		{"Bad-Name", postgres.ErrInvalidDatabaseName},
		{"", postgres.ErrInvalidDatabaseName},
	}
	for _, tt := range tests {
		start := time.Now()
		_, err := postgres.New(t.Context(), cfgFor(t, "", tt.name))
		if !errors.Is(err, tt.want) {
			t.Errorf("%q: got %v, want %v", tt.name, err, tt.want)
		}
		if time.Since(start) > time.Second {
			t.Errorf("%q: New took too long, so it possibly connected", tt.name)
		}
	}
}

// U26
func TestLoadRequiresDatabaseName(t *testing.T) {
	t.Setenv("SVC_DATABASE__URL", "postgres://u:p@h/db")
	var cfg struct {
		Database postgres.Config `envPrefix:"DATABASE__"`
	}
	err := config.Load("SVC_", &cfg)
	if err == nil || !strings.Contains(err.Error(), "SVC_DATABASE__NAME") {
		t.Fatalf("got %v", err)
	}
}

// U36
func TestConnString(t *testing.T) {
	c := postgres.Config{URL: "postgres://u:s3cr3t@h:5432?sslmode=disable", Database: "a"}
	got, err := c.ConnString()
	if err != nil || got.Reveal() != "postgres://u:s3cr3t@h:5432/a?sslmode=disable" { //nolint:gosec // test data
		t.Fatalf("no database: %q %v", got.Reveal(), err)
	}
	c.URL = "postgresql://u:s3cr3t@h:5432/a"
	got, err = c.ConnString()
	if err != nil || got.Reveal() != "postgresql://u:s3cr3t@h:5432/a" { //nolint:gosec // test data
		t.Fatalf("same database: %q %v", got.Reveal(), err)
	}
	c.URL = "postgres://u:s3cr3t@h:5432/b"
	_, err = c.ConnString()
	if !errors.Is(err, postgres.ErrDatabaseMismatch) || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("other database: %v", err)
	}
	c.URL, c.Database = "postgres://u:s3cr3t@h:5432", "postgres"
	_, err = c.ConnString()
	if !errors.Is(err, postgres.ErrSystemDatabase) || strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("system database: %v", err)
	}
}

// U38
func TestLoadDefaultsAndValues(t *testing.T) {
	t.Setenv("SVC_DATABASE__URL", "postgres://u:p@h/db")
	t.Setenv("SVC_DATABASE__NAME", "db")
	var cfg struct {
		Database postgres.Config `envPrefix:"DATABASE__"`
	}
	if err := config.Load("SVC_", &cfg); err != nil {
		t.Fatal(err)
	}
	d := cfg.Database
	if d.MaxConns != 10 || d.MinConns != 0 || d.ConnectTimeout != 5*time.Second {
		t.Fatalf("defaults: %+v", d)
	}
	t.Setenv("SVC_DATABASE__MAX_CONNS", "20")
	t.Setenv("SVC_DATABASE__MIN_CONNS", "2")
	t.Setenv("SVC_DATABASE__CONNECT_TIMEOUT", "3s")
	if err := config.Load("SVC_", &cfg); err != nil {
		t.Fatal(err)
	}
	d = cfg.Database
	if d.MaxConns != 20 || d.MinConns != 2 || d.ConnectTimeout != 3*time.Second {
		t.Fatalf("values: %+v", d)
	}
}

// U39
func TestInvalidURLForms(t *testing.T) {
	for _, u := range []string{"host=a dbname=b password=p", "mysql://u:p@a/b"} {
		c := postgres.Config{URL: config.Secret(u), Database: "b"}
		_, err := postgres.New(t.Context(), c)
		if !errors.Is(err, postgres.ErrInvalidURL) || strings.Contains(err.Error(), "password") || strings.Contains(err.Error(), u) {
			t.Errorf("New %q: %v", u, err)
		}
		if _, err := c.ConnString(); !errors.Is(err, postgres.ErrInvalidURL) {
			t.Errorf("ConnString %q: %v", u, err)
		}
	}
}

// U40
func TestConnStringIgnoresPGDATABASE(t *testing.T) {
	t.Setenv("PGDATABASE", "b")
	c := postgres.Config{URL: "postgres://u:p@h:5432", Database: "a"} //nolint:gosec // test data
	got, err := c.ConnString()
	if err != nil || !strings.HasSuffix(got.Reveal(), "/a") {
		t.Fatalf("got %q %v", got.Reveal(), err)
	}
}
