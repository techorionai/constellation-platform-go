package health_test

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"

	clientpg "github.com/techorionai/constellation-platform-go/clients/postgres"
	clientvk "github.com/techorionai/constellation-platform-go/clients/valkey"
	"github.com/techorionai/constellation-platform-go/health"
	healthpg "github.com/techorionai/constellation-platform-go/health/postgres"
	healthvk "github.com/techorionai/constellation-platform-go/health/valkey"
)

type alwaysOK struct{}

func (alwaysOK) Name() string                { return "cache" }
func (alwaysOK) Check(context.Context) error { return nil }

// ExampleNewHandler shows a custom checker. Each type with Name and Check is a
// checker.
func ExampleNewHandler() {
	state := health.NewState(alwaysOK{})
	handler := health.NewHandler(state, health.Config{})

	get := func(path string) string {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil))
		body, _ := io.ReadAll(rec.Result().Body)
		return fmt.Sprintf("%d %s", rec.Code, body)
	}
	fmt.Print(get("/readyz"))
	state.MarkStarted()
	fmt.Print(get("/readyz"))
	// Output:
	// 503 {"type":"about:blank","title":"Service Unavailable","status":503,"detail":"The service is not ready.","code":"service_not_ready"}
	// 200 {"status":"ready","checks":{"cache":"ok"}}
}

// ExampleNewServer shows the admin server and the graceful stop. It has no
// Output comment, because it listens on a port.
func ExampleNewServer() {
	ctx := context.Background()
	state := health.NewState()
	srv := health.NewServer(health.Config{Addr: ":9090"}, state)
	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Print(err)
		}
	}()

	// ... run the migrations and start the API server ...
	state.MarkStarted()

	// On a stop signal:
	_ = srv.Drain(ctx)    // /readyz is 503 during DrainDelay
	_ = srv.Shutdown(ctx) // the admin server stops last
}

// ExampleNewState shows the checker cases. It has no Output comment, because it
// needs PostgreSQL and Valkey servers.
func ExampleNewState() {
	ctx := context.Background()
	var (
		db *clientpg.Client
		kv *clientvk.Client
	)
	pgCheck, err := healthpg.NewChecker(ctx, db.Pool)
	if err != nil {
		log.Fatal(err)
	}
	defer pgCheck.Close()

	states := []*health.State{
		// PostgreSQL only
		health.NewState(pgCheck),
		// Valkey only
		health.NewState(healthvk.NewChecker(kv.Raw)),
		// Both
		health.NewState(pgCheck, healthvk.NewChecker(kv.Raw)),
		// None, for a service that has no database
		health.NewState(),
	}
	for _, state := range states {
		state.MarkStarted()
	}
}
