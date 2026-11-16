package transaction_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/resources/test/mock"
	"github.com/techorionai/constellation-platform-go/transaction"
)

// U28
func TestResolve(t *testing.T) {
	for _, opt := range []transaction.Option{
		transaction.Name("Bad-Name"),
		transaction.Name(strings.Repeat("a", 65)),
		transaction.Isolation(transaction.Level(99)),
	} {
		if _, err := transaction.Resolve(opt); !errors.Is(err, transaction.ErrInvalidOption) {
			t.Fatalf("err = %v", err)
		}
	}
	s, err := transaction.Resolve(transaction.ReadOnly(), transaction.Isolation(transaction.Serializable), transaction.Name("create_order"))
	if err != nil || !s.ReadOnly || s.Isolation != transaction.Serializable || s.Name != "create_order" {
		t.Fatalf("got %+v, %v", s, err)
	}
	s, err = transaction.Resolve()
	if err != nil || s.ReadOnly || s.Isolation != transaction.ReadCommitted || s.Name != "unnamed" {
		t.Fatalf("got %+v, %v", s, err)
	}
	if _, err := transaction.Resolve(transaction.Name(strings.Repeat("a", 64))); err != nil {
		t.Fatalf("64 characters: %v", err)
	}
}

// U19
func TestCommandWithMockTransactor(t *testing.T) {
	tx := &mock.Transactor{}
	errX := errors.New("x")
	command := func(ctx context.Context) error {
		return tx.WithinTx(ctx, func(context.Context) error { return errX })
	}
	if err := command(t.Context()); !errors.Is(err, errX) {
		t.Fatalf("err = %v", err)
	}
	if tx.WithinTxCalls() != 1 {
		t.Fatalf("calls = %d", tx.WithinTxCalls())
	}
}

// U34
func TestMockTransactor(t *testing.T) {
	ctx := t.Context()
	tx := &mock.Transactor{}

	runs := 0
	hook := func(context.Context) { runs++ }

	// The hook runs one time after the first call.
	if err := tx.WithinTx(ctx, func(ctx context.Context) error {
		tx.AfterCommit(ctx, hook)
		if runs != 0 {
			t.Fatal("the hook ran before the end of fn")
		}
		return nil
	}); err != nil || runs != 1 {
		t.Fatalf("err %v, runs %d", err, runs)
	}

	// The hook does not run if fn returns an error.
	errX := errors.New("x")
	if err := tx.WithinTx(ctx, func(ctx context.Context) error {
		tx.AfterCommit(ctx, hook)
		return errX
	}); !errors.Is(err, errX) || runs != 1 {
		t.Fatalf("err %v, runs %d", err, runs)
	}

	// AfterCommit outside WithinTx runs the hook at once.
	tx.AfterCommit(ctx, hook)
	if runs != 2 {
		t.Fatalf("runs = %d", runs)
	}

	// An invalid option returns an error and does not call fn.
	called := false
	err := tx.WithinTx(ctx, func(context.Context) error { called = true; return nil }, transaction.Name("Bad-Name"))
	if !errors.Is(err, transaction.ErrInvalidOption) || called {
		t.Fatalf("err %v, called %v", err, called)
	}

	// A nested hook runs one time, after the outer fn returns.
	nestedRuns, ranBeforeReturn := 0, false
	if err := tx.WithinTx(ctx, func(ctx context.Context) error {
		if err := tx.WithinTx(ctx, func(ctx context.Context) error {
			tx.AfterCommit(ctx, func(context.Context) { nestedRuns++ })
			return nil
		}, transaction.Name("inner")); err != nil {
			return err
		}
		ranBeforeReturn = nestedRuns != 0
		return nil
	}); err != nil || nestedRuns != 1 || ranBeforeReturn {
		t.Fatalf("err %v, nested runs %d, ran before return %v", err, nestedRuns, ranBeforeReturn)
	}

	// An inner call with ReadOnly returns ErrNestedOptions.
	if err := tx.WithinTx(ctx, func(ctx context.Context) error {
		return tx.WithinTx(ctx, func(context.Context) error { return nil }, transaction.ReadOnly())
	}); !errors.Is(err, transaction.ErrNestedOptions) {
		t.Fatalf("err = %v", err)
	}
}
