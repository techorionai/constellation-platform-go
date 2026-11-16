package platform_test

import (
	"context"
	"errors"
	"fmt"
	"log"

	"github.com/techorionai/constellation-platform-go/retention"
	"github.com/techorionai/constellation-platform-go/secrets"
)

// Example_admin has no Output comment, because it needs a database. It is the
// sample of an admin command for the rotation of the master key and for a
// manual retention run. The service gives its own entry point, for example a
// subcommand.
func Example_admin() {
	ctx := context.Background()
	var (
		svc    *secrets.Service
		runner *retention.Runner
	)

	// Rotation, step 3: rewrap until Conflicts and Failed are 0.
	res, err := svc.Rewrap(ctx, 100)
	fmt.Printf("rewrapped=%d conflicts=%d failed=%d\n", res.Rewrapped, res.Conflicts, res.Failed)
	if err != nil {
		log.Fatal(err)
	}
	if res.Conflicts > 0 || res.Failed > 0 {
		fmt.Println("run the command again, and examine the failed records before you remove the old key")
	}

	// A manual run of one retention job.
	n, err := runner.RunOnce(ctx, "audit")
	switch {
	case errors.Is(err, retention.ErrLocked):
		fmt.Println("another instance runs the job now, so this call did nothing")
	case err != nil:
		log.Fatal(err)
	default:
		fmt.Printf("deleted %d rows\n", n)
	}
}
