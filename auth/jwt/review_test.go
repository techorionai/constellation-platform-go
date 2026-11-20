package jwt_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.uber.org/goleak"

	"github.com/techorionai/constellation-platform-go/auth"
	authjwt "github.com/techorionai/constellation-platform-go/auth/jwt"
	"github.com/techorionai/constellation-platform-go/config"
	"github.com/techorionai/constellation-platform-go/resources/test/issuer"
)

// U17b: a token with an unknown kid starts a window. A token with a new key gets
// a retryable error inside the window and passes after it.
func TestRotationInsideAttackerWindow(t *testing.T) {
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{})
	f.clock.Advance(31 * time.Second)
	_, err := f.verify(t, issuer.Claims{SigningKey: issuer.GenerateKey(t), Kid: "unknown"})
	wantErr(t, err, auth.ErrInvalidToken)
	f.iss.Rotate(t)
	f.clock.Advance(5 * time.Second)
	_, err = f.verify(t, issuer.Claims{})
	wantErr(t, err, auth.ErrKeysUnavailable)
	f.clock.Advance(31 * time.Second)
	f.mustVerify(t, issuer.Claims{})
}

// U25a
func TestInsecureHTTPConfig(t *testing.T) {
	cases := []struct {
		name  string
		cfg   authjwt.Config
		allow bool
		ok    bool
		field string
	}{
		{"http issuer", authjwt.Config{Issuer: "http://id.example", Audience: "a"}, false, false, "Issuer"},
		{"http issuer allowed", authjwt.Config{Issuer: "http://id.example", Audience: "a"}, true, true, ""},
		{"https issuer, http jwks", authjwt.Config{Issuer: "https://id.example", Audience: "a", JWKSURL: "http://id.example/k"}, false, false, "JWKSURL"},
		{"https issuer, http jwks allowed", authjwt.Config{Issuer: "https://id.example", Audience: "a", JWKSURL: "http://id.example/k"}, true, true, ""},
		{"https", authjwt.Config{Issuer: "https://id.example", Audience: "a"}, false, true, ""},
		{"ftp allowed", authjwt.Config{Issuer: "ftp://id.example", Audience: "a"}, true, false, "Issuer"},
		{"file jwks allowed", authjwt.Config{Issuer: "https://id.example", Audience: "a", JWKSURL: "file:///k"}, true, false, "JWKSURL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.AllowInsecureHTTP = tc.allow
			v, err := authjwt.New(tc.cfg)
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				v.Close()
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error = %v, want one that names %s", err, tc.field)
			}
		})
	}

	type holder struct {
		Auth authjwt.Config `envPrefix:"AUTH__"`
	}
	t.Setenv("SVC_AUTH__ISSUER", "https://id.example")
	t.Setenv("SVC_AUTH__AUDIENCE", "demo")
	t.Setenv("SVC_AUTH__ALLOW_INSECURE_HTTP", "true")
	var h holder
	if err := config.Load("SVC_", &h); err != nil {
		t.Fatal(err)
	}
	if !h.Auth.AllowInsecureHTTP {
		t.Error("AUTH__ALLOW_INSECURE_HTTP is not loaded")
	}
}

// U25b
func TestNoRedirects(t *testing.T) {
	for _, name := range []string{"default client", "custom client"} {
		t.Run(name, func(t *testing.T) {
			iss := issuer.New(t)
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, iss.URL(), http.StatusFound)
			}))
			t.Cleanup(redirect.Close)
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			opts := []authjwt.Option{authjwt.WithMeterProvider(mp)}
			if name == "custom client" {
				// This client follows redirects, but the verifier must not use that.
				opts = append(opts, authjwt.WithHTTPClient(&http.Client{Timeout: 5 * time.Second}))
			}
			v, err := authjwt.New(authjwt.Config{
				Issuer: iss.Issuer(), Audience: audience, JWKSURL: redirect.URL, AllowInsecureHTTP: true,
			}, opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			_, err = v.Verify(context.Background(), iss.Mint(t, issuer.Claims{}))
			wantErr(t, err, auth.ErrKeysUnavailable)
			if got := iss.Fetches(); got != 0 {
				t.Errorf("the test issuer got %d requests, want 0", got)
			}
			f := &fixture{reader: reader}
			if got := f.fetchOutcomes(t); got["http_error"] != 1 {
				t.Errorf("fetch outcomes %v", got)
			}
		})
	}
}

// U25c
func TestLeewayLimits(t *testing.T) {
	base := authjwt.Config{Issuer: "https://id.example", Audience: "a"}
	for _, d := range []time.Duration{-time.Second, 2*time.Minute + time.Second} {
		c := base
		c.Leeway = d
		if _, err := authjwt.New(c); err == nil || !strings.Contains(err.Error(), "Leeway") {
			t.Errorf("leeway %v: error = %v", d, err)
		}
	}
	c := base
	c.Leeway = 2 * time.Minute
	v, err := authjwt.New(c)
	if err != nil {
		t.Fatalf("leeway 2m: %v", err)
	}
	v.Close()

	// Leeway 0 means the default of 30 s.
	f := newFixture(t)
	f.mustVerify(t, issuer.Claims{Expires: f.clock.Now().Add(-10 * time.Second)})
	_, err = f.verify(t, issuer.Claims{Expires: f.clock.Now().Add(-40 * time.Second)})
	wantErr(t, err, auth.ErrExpired)
}

// R3: Start after Close does not start a goroutine.
func TestStartAfterClose(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	tr := &http.Transport{}
	f := newFixture(t, authjwt.WithHTTPClient(&http.Client{Transport: tr, Timeout: 5 * time.Second}))
	f.v.SetTick(10 * time.Millisecond)
	f.v.Close()
	f.v.Start(context.Background())
	f.v.Close()
	time.Sleep(50 * time.Millisecond)
	if got := f.iss.Fetches(); got != 0 {
		t.Errorf("fetches = %d, want 0", got)
	}
	tr.CloseIdleConnections()
	f.iss.Close()
}

// slowJWKS is a JWKS server that does not answer until release is closed or the
// request ends.
func slowJWKS(t *testing.T, iss *issuer.Issuer) (url string, release func()) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	rel := func() { once.Do(func() { close(ch) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-ch:
		case <-r.Context().Done():
			return
		}
		http.Redirect(w, r, iss.URL(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(func() { rel(); srv.Close() })
	return srv.URL, rel
}

// R6: Close does not wait for a running background fetch.
func TestCloseDuringFetch(t *testing.T) {
	iss := issuer.New(t)
	url, _ := slowJWKS(t, iss)
	v, err := authjwt.New(authjwt.Config{
		Issuer: iss.Issuer(), Audience: audience, JWKSURL: url, AllowInsecureHTTP: true, HTTPTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	v.SetTick(10 * time.Millisecond)
	v.Start(context.Background())
	time.Sleep(200 * time.Millisecond) // the loop is now in the fetch
	start := time.Now()
	v.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Close took %v", d)
	}
}

// R7: Verify returns when the context of the caller ends.
func TestVerifyContextEnds(t *testing.T) {
	iss := issuer.New(t)
	url, _ := slowJWKS(t, iss)
	v, err := authjwt.New(authjwt.Config{
		Issuer: iss.Issuer(), Audience: audience, JWKSURL: url, AllowInsecureHTTP: true, HTTPTimeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = v.Verify(ctx, iss.Mint(t, issuer.Claims{}))
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Verify took %v", d)
	}
	wantErr(t, err, auth.ErrKeysUnavailable)
	wantErr(t, err, context.DeadlineExceeded)
}

// R9: time claims out of range are rejected.
func TestTimeClaimsOutOfRange(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"nbf", "iat", "exp"} {
		for _, val := range []float64{1e300, -1e300, 1e12} {
			_, err := f.verify(t, issuer.Claims{Extra: map[string]any{name: val}})
			wantErr(t, err, auth.ErrInvalidToken)
		}
	}
}

// R19: claim names match with exact case.
func TestClaimNamesCaseSensitive(t *testing.T) {
	f := newFixture(t)
	p, err := f.verify(t, issuer.Claims{
		Remove: []string{"scope"}, Extra: map[string]any{"SCOPE": "demo:ticket:admin"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Scopes) != 0 {
		t.Errorf("scopes %v, want none", p.Scopes)
	}
	_, err = f.verify(t, issuer.Claims{Remove: []string{"sub"}, Extra: map[string]any{"SUB": "u1"}})
	wantErr(t, err, auth.ErrInvalidToken)
	_, err = f.verify(t, issuer.Claims{Remove: []string{"exp"}, Extra: map[string]any{"EXP": 4102444800}})
	wantErr(t, err, auth.ErrInvalidToken)
	_, err = f.verify(t, issuer.Claims{Extra: map[string]any{"ACT": map[string]any{"sub": "x", "principal_type": "user"}}})
	if err != nil {
		t.Fatal(err)
	}
}

// R15: concurrent calls share one fetch.
func TestConcurrentVerify(t *testing.T) {
	f := newFixture(t)
	run := func(mint func() string) {
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < 30; i++ {
			tok := mint()
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, err := f.v.Verify(context.Background(), tok)
				if err != nil && !strings.Contains(err.Error(), "auth:") {
					t.Errorf("verify: %v", err)
				}
			}()
		}
		close(start)
		wg.Wait()
	}
	// Empty cache.
	run(func() string { return f.iss.Mint(t, issuer.Claims{}) })
	if got := f.iss.Fetches(); got != 1 {
		t.Errorf("fetches with an empty cache = %d, want 1", got)
	}
	// Unknown kid after the rate limit window.
	f.clock.Advance(31 * time.Second)
	key := issuer.GenerateKey(t)
	run(func() string { return f.iss.Mint(t, issuer.Claims{SigningKey: key, Kid: "unknown"}) })
	if got := f.iss.Fetches(); got != 2 {
		t.Errorf("fetches after unknown kid = %d, want 2", got)
	}
}
