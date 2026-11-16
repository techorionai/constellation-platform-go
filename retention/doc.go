// Package retention deletes expired data with background jobs.
//
// A [Job] has a name, a [Config], a default period, and a [Purger]. The
// [Purger] port deletes at most a given number of items that are older than a
// given time. The [Locker] port makes sure that only one instance of a service
// runs a job at one time. The [Runner] runs each job in its own goroutine. A
// tick takes the lock, calls [Purger.Purge] in batches until it deletes less
// than a batch, and releases the lock. The adapters are in the package
// retention/postgres. This package does not import a database driver.
//
// A service puts one [Config] for each job in its config struct, and starts the
// runner after the migrations:
//
//	var cfg struct {
//		AuditRetention retention.Config `envPrefix:"RETENTION__AUDIT__"`
//	}
//	if err := config.Load("IDENTITY_", &cfg); err != nil {
//		return err
//	}
//	runner, err := retention.NewRunner(locker, []retention.Job{{
//		Name:          "audit",
//		Config:        cfg.AuditRetention,
//		DefaultPeriod: 60 * 24 * time.Hour,
//		Purger:        purger,
//	}}, retention.RunnerOptions{})
//	if err != nil {
//		return err
//	}
//	go runner.Run(ctx)
//
// Each tick makes the span retention.tick and records the metrics
// retention.deleted, retention.duration, and retention.last_success.
package retention
