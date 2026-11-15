// Package health gives the liveness and readiness endpoints of a service on a
// separate admin port.
//
// [Checker] is the port of a dependency check. The adapters are in the packages
// health/postgres and health/valkey. A service gives only the checkers that it
// needs, and a service with no database can give none. [State] holds the
// readiness state. [NewHandler] serves /livez and /readyz. [Server] runs the
// handler on the admin port.
//
// /readyz is 503 until the service calls [State.MarkStarted], and 503 again
// after [State.MarkDraining]. A 503 body is a problem document
// (application/problem+json). It never contains the error text of a checker.
// The handler writes that text to the log at the warn level.
//
//	state := health.NewState(pgCheck, valkeyCheck)
//	srv := health.NewServer(cfg.Admin, state)
//	go srv.ListenAndServe()
//	// ... migrate.Run, start the API server ...
//	state.MarkStarted()
//	// ... on a stop signal ...
//	srv.Drain(ctx)
//
// The admin server has no authentication. Do not publish the admin port at the
// edge.
package health
