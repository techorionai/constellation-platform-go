//go:build integration

package health_test

import (
	"context"
	"net"
	"net/http"
	"testing"
	"testing/fstest"
	"time"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	clientvk "github.com/techorionai/constellation-platform-go/clients/valkey"
	"github.com/techorionai/constellation-platform-go/health"
	healthpg "github.com/techorionai/constellation-platform-go/health/postgres"
	healthvk "github.com/techorionai/constellation-platform-go/health/valkey"
	"github.com/techorionai/constellation-platform-go/migrate"
	migratepg "github.com/techorionai/constellation-platform-go/migrate/postgres"
	"github.com/techorionai/constellation-platform-go/resources/test/containers"
)

// freeAddr returns a local address with a free port.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func dial(t *testing.T, addr string, timeout time.Duration) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
}

func status(t *testing.T, addr, path string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	return resp.StatusCode
}

// serve starts the server and waits until /livez answers.
func serve(t *testing.T, srv *health.Server, addr string) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := dial(t, addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return done
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not start: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// I10: follow the startup order of docs/usage.md.
func TestIntegrationStartupOrder(t *testing.T) {
	pg := containers.Postgres(t)
	vk := containers.Valkey(t)
	dbURL, name := pg.NewDatabase(t)
	user, password := vk.NewACLUser(t, "svc")

	db, err := clientpg.New(t.Context(), clientpg.Config{URL: dbURL, Database: name, MaxConns: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	kv, err := clientvk.New(t.Context(), clientvk.Config{
		Address: vk.Address(), Username: user, Password: password, KeyPrefix: "svc",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer kv.Close()
	pgCheck, err := healthpg.NewChecker(t.Context(), db.Pool)
	if err != nil {
		t.Fatal(err)
	}
	defer pgCheck.Close()

	state := health.NewState(pgCheck, healthvk.NewChecker(kv.Raw))
	addr := freeAddr(t)
	srv := health.NewServer(health.Config{Addr: addr}, state)
	done := serve(t, srv, addr)

	if got := status(t, addr, "/livez"); got != 200 {
		t.Fatalf("livez = %d", got)
	}
	if got := status(t, addr, "/readyz"); got != 503 {
		t.Fatalf("readyz before migrate = %d", got)
	}

	fsys := fstest.MapFS{"1_a.up.sql": {Data: []byte("CREATE TABLE a (id int);")}}
	migrator, err := migratepg.NewMigrator(db.Pool, fsys, migratepg.MigratorOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Run(t.Context(), migratepg.NewLocker(db.Pool, ""), migrator, migrate.Options{}); err != nil {
		t.Fatal(err)
	}
	state.MarkStarted()
	if got := status(t, addr, "/readyz"); got != 200 {
		t.Fatalf("readyz after start = %d", got)
	}

	if err := srv.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// I18
func TestIntegrationShutdown(t *testing.T) {
	state := health.NewState()
	addr := freeAddr(t)
	srv := health.NewServer(health.Config{Addr: addr}, state)
	done := serve(t, srv, addr)

	if err := srv.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("ListenAndServe: %v", err)
	}
	conn, err := dial(t, addr, time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("the port still accepts connections")
	}
}
