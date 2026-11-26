package platform_test

import (
	"context"
	"log"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/cache"
	cachevk "github.com/techorionai/constellation-platform-go/cache/valkey"
	"github.com/techorionai/constellation-platform-go/clients/postgres"
	"github.com/techorionai/constellation-platform-go/clients/valkey"
	"github.com/techorionai/constellation-platform-go/decorators"
	"github.com/techorionai/constellation-platform-go/executor"
	"github.com/techorionai/constellation-platform-go/outbox"
	outboxpg "github.com/techorionai/constellation-platform-go/outbox/postgres"
	streamsvk "github.com/techorionai/constellation-platform-go/streams/valkey"
	"github.com/techorionai/constellation-platform-go/transaction"
	txpg "github.com/techorionai/constellation-platform-go/transaction/postgres"
)

type renameOrder struct {
	ID   string
	Name string
}

type order struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// orderRepo is a repository adapter. It gets its connection with postgres.Conn
// for each call, so it joins the transaction of the caller.
type orderRepo struct {
	pool   *pgxpool.Pool
	events outbox.Writer
}

func (r orderRepo) rename(ctx context.Context, id, name string) error {
	_, err := postgres.Conn(ctx, r.pool).Exec(ctx, `UPDATE orders SET name = $2 WHERE id = $1`, id, name)
	return err
}

// writeOutbox writes the event in the same transaction as the change. The actor
// comes from the verified principal of the request.
func (r orderRepo) writeOutbox(ctx context.Context, id string) error {
	p, _ := auth.PrincipalFromContext(ctx)
	return r.events.Write(ctx, outbox.Event{
		Type:    "orders.order.renamed",
		Subject: id,
		Data:    map[string]any{"id": id},
		Actor:   outbox.ActorFromPrincipal(p),
	})
}

// Example_command shows a command with a repository and the outbox in one
// WithinTx, a relay wake-up and a cache Delete through AfterCommit, and the
// tracing and logging decorators. It has no Output comment, because it needs
// PostgreSQL and Valkey servers.
func Example_command() {
	ctx := context.Background()
	var (
		db *postgres.Client
		kv *valkey.Client
	)

	events, err := outboxpg.NewWriter(db.Pool, outbox.Config{App: "orders", Types: []string{"orders.order.renamed"}})
	if err != nil {
		log.Fatal(err)
	}
	relay, err := outbox.NewRelay(outboxpg.NewStore(db.Pool), streamsvk.NewPublisher(kv.Raw, "orders"), outbox.RelayOptions{})
	if err != nil {
		log.Fatal(err)
	}
	go func() { _ = relay.Run(ctx) }()

	repo := orderRepo{pool: db.Pool, events: events}
	tx := txpg.NewTransactor(db.Pool)
	orders := cache.New[order](cachevk.NewStore(kv), "order")

	rename := executor.Func[renameOrder](func(ctx context.Context, p renameOrder) error {
		return tx.WithinTx(ctx, func(ctx context.Context) error {
			if err := repo.rename(ctx, p.ID, p.Name); err != nil {
				return err
			}
			if err := repo.writeOutbox(ctx, p.ID); err != nil {
				return err
			}
			// Delete the cache entry after the commit. Then another request
			// cannot store the old value again before the commit.
			tx.AfterCommit(ctx, func(ctx context.Context) {
				relay.Notify()
				_ = orders.Delete(ctx, p.ID)
			})
			return nil
		}, transaction.Name("rename_order"))
	})

	// Innermost first: the command, then logging, then tracing.
	var cmd executor.Executor[renameOrder] = rename
	cmd = decorators.ApplyLoggingNoResult(cmd, "RenameOrder")
	cmd = decorators.ApplyTracingNoResult(cmd, "RenameOrder")

	if err := cmd.Execute(ctx, renameOrder{ID: "o1", Name: "New name"}); err != nil {
		log.Fatal(err)
	}

	// A read path uses GetOrLoad.
	o, err := orders.GetOrLoad(ctx, "o1", time.Minute, func(ctx context.Context) (order, error) {
		var out order
		err := postgres.Conn(ctx, db.Pool).QueryRow(ctx, `SELECT id, name FROM orders WHERE id = $1`, "o1").Scan(&out.ID, &out.Name)
		return out, err
	})
	if err != nil {
		log.Fatal(err)
	}
	log.Print(o.Name)
}
