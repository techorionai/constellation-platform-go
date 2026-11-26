package introspect_test

import (
	"context"
	"testing"
	"time"

	"github.com/techorionai/constellation-platform-go/auth"
)

// Issue 18 U13: an API token principal has no amr and no auth_time, and CheckMFA refuses it.
func TestU13IntrospectionNoMFA(t *testing.T) {
	f := newFixture(t, setup{})
	p, err := f.v.Verify(context.Background(), token(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.AuthMethods) != 0 || !p.AuthTime.IsZero() {
		t.Errorf("fields %v %v", p.AuthMethods, p.AuthTime)
	}
	if err := auth.CheckMFA(p, 5*time.Minute, f.clock.now()); err == nil {
		t.Error("CheckMFA passed")
	}
}
