package postgres_test

import (
	"testing"

	"github.com/techorionai/constellation-platform-go/secrets"
	secretspg "github.com/techorionai/constellation-platform-go/secrets/postgres"
)

// ListNames checks the limit before it uses the pool, so a nil pool shows that
// no SQL runs.
func TestListNamesRejectsBadLimit(t *testing.T) {
	repo := secretspg.NewRepository(nil)
	for _, limit := range []int{0, -1} {
		if _, err := repo.ListNames(t.Context(), secrets.ListParams{Limit: limit}); err == nil {
			t.Fatalf("limit %d: no error", limit)
		}
	}
}
