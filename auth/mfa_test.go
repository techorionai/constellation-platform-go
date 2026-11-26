package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/requestid"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

const wantChallenge300 = `Bearer error="insufficient_user_authentication", error_description="A fresh MFA login is necessary", max_age="300"`

// mfaRun runs the middleware with the principal in the context.
func mfaRun(t *testing.T, mw func(http.Handler) http.Handler, p *auth.Principal) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	ran := false
	h := requestid.Middleware()(mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusNoContent)
	})))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	if p != nil {
		req = req.WithContext(auth.ContextWithPrincipal(req.Context(), *p))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, ran
}

func mfaP(age time.Duration, methods ...string) *auth.Principal {
	p := auth.Principal{ID: "u1", Type: auth.PrincipalUser, AuthMethods: methods}
	if age != 0 {
		p.AuthTime = time.Now().Add(-age)
	}
	return &p
}

func wantRefusal(t *testing.T, rec *httptest.ResponseRecorder, ran bool) {
	t.Helper()
	if rec.Code != 401 || ran {
		t.Fatalf("status %d ran %v body %s", rec.Code, ran, rec.Body)
	}
	if decode(t, rec).Code != "reauthentication_required" {
		t.Errorf("body %s", rec.Body)
	}
}

// Issue 18 U6
func TestU6RequireMFAPass(t *testing.T) {
	rec, ran := mfaRun(t, auth.RequireMFA(5*time.Minute), mfaP(2*time.Minute, "pwd", "otp", "mfa"))
	if rec.Code != 204 || !ran {
		t.Fatalf("status %d ran %v", rec.Code, ran)
	}
}

// Issue 18 U7
func TestU7RequireMFATooOld(t *testing.T) {
	rec, ran := mfaRun(t, auth.RequireMFA(5*time.Minute), mfaP(6*time.Minute, "mfa"))
	wantRefusal(t, rec, ran)
	if got := rec.Header().Get("WWW-Authenticate"); got != wantChallenge300 {
		t.Errorf("header %q", got)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("content type %q", ct)
	}
	p := decode(t, rec)
	if p.Type != "about:blank" || p.Title != "Reauthentication required" || p.Status != 401 || p.RequestID == "" {
		t.Errorf("body %+v", p)
	}
}

// Issue 18 U8
func TestU8RequireMFANoMFA(t *testing.T) {
	rec, ran := mfaRun(t, auth.RequireMFA(5*time.Minute), mfaP(time.Minute, "pwd"))
	wantRefusal(t, rec, ran)
}

// Issue 18 U9
func TestU9RequireMFAZeroMaxAge(t *testing.T) {
	rec, ran := mfaRun(t, auth.RequireMFA(0), mfaP(24*time.Hour, "mfa"))
	if rec.Code != 204 || !ran {
		t.Fatalf("status %d ran %v", rec.Code, ran)
	}
}

// Issue 18 U10
func TestU10RequireMFAZeroNoMFA(t *testing.T) {
	rec, ran := mfaRun(t, auth.RequireMFA(0), mfaP(0, "pwd"))
	wantRefusal(t, rec, ran)
	if strings.Contains(rec.Header().Get("WWW-Authenticate"), "max_age") {
		t.Errorf("header %q", rec.Header().Get("WWW-Authenticate"))
	}
}

// Issue 18 U11
func TestU11RequireMFAAct(t *testing.T) {
	p := mfaP(time.Second, "mfa")
	p.Act = &auth.Actor{ID: "a1", Type: auth.PrincipalAgent}
	rec, ran := mfaRun(t, auth.RequireMFA(5*time.Minute), p)
	wantRefusal(t, rec, ran)
}

// Issue 18 U12
func TestU12RequireMFANoPrincipal(t *testing.T) {
	logs := captureLogs(t)
	rec, ran := mfaRun(t, auth.RequireMFA(5*time.Minute), nil)
	if rec.Code != 500 || decode(t, rec).Code != "auth_not_configured" || ran {
		t.Fatalf("status %d ran %v body %s", rec.Code, ran, rec.Body)
	}
	if n := strings.Count(logs.String(), `"level":"ERROR"`); n != 1 {
		t.Errorf("error records: %d", n)
	}
}

// Issue 18 U13
func TestU13APIToken(t *testing.T) {
	p := mfaP(time.Minute, "mfa")
	p.TokenType = auth.TokenTypeAPI
	if err := auth.CheckMFA(*p, 5*time.Minute, time.Now()); !errors.Is(err, auth.ErrReauthenticationRequired) {
		t.Errorf("err %v", err)
	}
	rec, ran := mfaRun(t, auth.RequireMFA(5*time.Minute), mfaP(0, "pwd"))
	wantRefusal(t, rec, ran)
}

// Issue 18 U14
func TestU14InclusiveLimit(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	p := auth.Principal{AuthMethods: []string{"mfa"}, AuthTime: now.Add(-5 * time.Minute)}
	if err := auth.CheckMFA(p, 5*time.Minute, now); err != nil {
		t.Errorf("exact limit: %v", err)
	}
	p.AuthTime = now.Add(-5*time.Minute - time.Second)
	if err := auth.CheckMFA(p, 5*time.Minute, now); !errors.Is(err, auth.ErrReauthenticationRequired) {
		t.Errorf("over limit: %v", err)
	}
}

// Issue 18 U15
func TestU15ZeroAuthTime(t *testing.T) {
	now := time.Now()
	p := auth.Principal{AuthMethods: []string{"mfa"}}
	if err := auth.CheckMFA(p, 5*time.Minute, now); !errors.Is(err, auth.ErrReauthenticationRequired) {
		t.Errorf("5m: %v", err)
	}
	if err := auth.CheckMFA(p, 0, now); err != nil {
		t.Errorf("0: %v", err)
	}
}

// Issue 18 U16
func TestU16FutureAndNegative(t *testing.T) {
	now := time.Now()
	p := auth.Principal{AuthMethods: []string{"mfa"}, AuthTime: now.Add(20 * time.Second)}
	if err := auth.CheckMFA(p, 5*time.Minute, now); err != nil {
		t.Errorf("future: %v", err)
	}
	if err := auth.CheckMFA(p, -time.Second, now); !errors.Is(err, auth.ErrReauthenticationRequired) {
		t.Errorf("negative: %v", err)
	}
}

// Issue 18 U17
func TestU17MFAChallenge(t *testing.T) {
	if got := auth.MFAChallenge(5 * time.Minute); got != wantChallenge300 {
		t.Errorf("5m: %q", got)
	}
	if got := auth.MFAChallenge(0); strings.Contains(got, "max_age") {
		t.Errorf("0: %q", got)
	}
	if got := auth.MFAChallenge(90500 * time.Millisecond); !strings.HasSuffix(got, `max_age="91"`) {
		t.Errorf("90500ms: %q", got)
	}
}

// Issue 18 U18
func TestU18WriteAndPanic(t *testing.T) {
	h := requestid.Middleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.WriteReauthenticationRequired(w, r, 5*time.Minute)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil))
	wantRefusal(t, rec, false)
	if rec.Header().Get("WWW-Authenticate") != wantChallenge300 {
		t.Errorf("header %q", rec.Header().Get("WWW-Authenticate"))
	}
	defer func() {
		if recover() == nil {
			t.Error("no panic")
		}
	}()
	auth.RequireMFA(-time.Second)
}

// Issue 18 U19
func TestU19NoMetric(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	v := &mock.Verifier{VerifyFunc: func(context.Context, string) (auth.Principal, error) {
		return *mfaP(0, "pwd"), nil
	}}
	e := &env{reader: reader}
	h := requestid.Middleware()(auth.Middleware(v, auth.WithMeterProvider(mp))(auth.RequireMFA(5 * time.Minute)(http.NotFoundHandler())))
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("status %d", rec.Code)
	}
	got := e.outcomes(t)
	if len(got) != 1 || got["ok"] != 1 {
		t.Errorf("outcomes %v", got)
	}
}

// Issue 18 U22
func TestU22MFAPrincipal(t *testing.T) {
	now := time.Now()
	p := mock.MFAPrincipal("u1", now)
	if err := auth.CheckMFA(p, 5*time.Minute, now); err != nil || !p.HasMFA() {
		t.Errorf("err %v mfa %v", err, p.HasMFA())
	}
}

func TestHasMFA(t *testing.T) {
	if (auth.Principal{AuthMethods: []string{"pwd"}}).HasMFA() || !(auth.Principal{AuthMethods: []string{"pwd", "mfa"}}).HasMFA() {
		t.Error("HasMFA")
	}
}
