package transaction_test

import (
	"context"
	"fmt"

	"github.com/techorionai/constellation-platform-go/resources/test/mock"
	"github.com/techorionai/constellation-platform-go/transaction"
)

func ExampleTransactor() {
	// A service uses the adapter of transaction/postgres. This sample uses the
	// mock, which follows the same rules for the hooks.
	var tx transaction.Transactor = &mock.Transactor{}
	ctx := context.Background()

	err := tx.WithinTx(ctx, func(ctx context.Context) error {
		tx.AfterCommit(ctx, func(context.Context) { fmt.Println("after commit") })
		fmt.Println("in transaction")
		return nil
	}, transaction.Name("create_order"))
	fmt.Println(err)
	// Output:
	// in transaction
	// after commit
	// <nil>
}

func ExampleResolve() {
	s, err := transaction.Resolve(transaction.ReadOnly(), transaction.Isolation(transaction.Serializable))
	fmt.Println(s.ReadOnly, s.Isolation, s.Name, err)
	// Output: true serializable unnamed <nil>
}
