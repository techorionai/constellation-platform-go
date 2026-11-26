package jwt_test

import (
	"strings"
	"testing"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/resources/test/issuer"
)

// Issue 22 U4
func TestU4AppClaim(t *testing.T) {
	f := newFixture(t)
	svc := func(extra map[string]any) issuer.Claims {
		return issuer.Claims{PrincipalType: "service", Extra: extra}
	}
	if p := f.mustVerify(t, svc(map[string]any{"app": "helpdesk"})); p.App != "helpdesk" {
		t.Errorf("App = %q", p.App)
	}
	if p := f.mustVerify(t, svc(nil)); p.App != "" {
		t.Errorf("App = %q", p.App)
	}
	for _, v := range []any{"Bad-App", strings.Repeat("a", 33), 5, "", []any{"a"}} {
		_, err := f.verify(t, svc(map[string]any{"app": v}))
		wantErr(t, err, auth.ErrInvalidToken)
	}
	if p := f.mustVerify(t, svc(map[string]any{"app": strings.Repeat("a", 32)})); len(p.App) != 32 {
		t.Errorf("App = %q", p.App)
	}
	if p := f.mustVerify(t, issuer.Claims{Extra: map[string]any{"app": "helpdesk"}}); p.App != "" {
		t.Errorf("user App = %q", p.App)
	}
	if p := f.mustVerify(t, issuer.Claims{Extra: map[string]any{"app": 5}}); p.App != "" {
		t.Errorf("user App = %q", p.App)
	}
	act := map[string]any{"app": "helpdesk", "act": map[string]any{"sub": "u1", "principal_type": "user"}}
	if p := f.mustVerify(t, issuer.Claims{Extra: act}); p.App != "" {
		t.Errorf("act App = %q", p.App)
	}
}
