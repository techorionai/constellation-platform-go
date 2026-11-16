package secrets_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/techorionai/constellation-platform-go/resources/test/mock"
	"github.com/techorionai/constellation-platform-go/secrets"
)

func newService(t testing.TB, repo secrets.Repository) *secrets.Service {
	t.Helper()
	return secrets.NewService(testRing(t, "k1", 1), repo)
}

// U11
func TestPutStoresEnvelopeOnly(t *testing.T) {
	repo := &mock.SecretsRepository{}
	svc := newService(t, repo)
	plain := []byte("super-plaintext-value")
	if err := svc.Put(context.Background(), "llm/key", plain); err != nil {
		t.Fatal(err)
	}
	rec, ok := repo.Record("llm/key")
	if !ok {
		t.Fatal("the repository has no record")
	}
	for _, b := range [][]byte{rec.Envelope.WrappedDEK, rec.Envelope.Nonce, rec.Envelope.Ciphertext, []byte(rec.Envelope.KeyID)} {
		if bytes.Contains(b, plain) {
			t.Fatal("the envelope contains the plaintext")
		}
	}
	got, err := svc.Get(context.Background(), "llm/key")
	if err != nil || !bytes.Equal(got, plain) {
		t.Fatalf("got %q, err %v", got, err)
	}
}

// U12
func TestPutNames(t *testing.T) {
	svc := newService(t, &mock.SecretsRepository{})
	for _, name := range []string{"A", "../x", strings.Repeat("a", 201), "", "-a"} {
		if err := svc.Put(context.Background(), name, []byte("v")); !errors.Is(err, secrets.ErrInvalidName) {
			t.Fatalf("name %q: err = %v, want ErrInvalidName", name, err)
		}
	}
	if err := svc.Put(context.Background(), strings.Repeat("a", 200), []byte("v")); err != nil {
		t.Fatalf("200 characters: %v", err)
	}
}

// U13
func TestPutValueSize(t *testing.T) {
	svc := newService(t, &mock.SecretsRepository{})
	ctx := context.Background()
	if err := svc.Put(ctx, "a", make([]byte, 64*1024+1)); !errors.Is(err, secrets.ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if err := svc.Put(ctx, "a", make([]byte, 64*1024)); err != nil {
		t.Fatalf("64 KiB: %v", err)
	}
	if err := svc.Put(ctx, "a", nil); !errors.Is(err, secrets.ErrEmptyValue) {
		t.Fatalf("err = %v, want ErrEmptyValue", err)
	}
}

// U14
func TestGetNotFound(t *testing.T) {
	repo := &mock.SecretsRepository{GetFunc: func(context.Context, string) (secrets.Record, error) {
		return secrets.Record{}, secrets.ErrNotFound
	}}
	if _, err := newService(t, repo).Get(context.Background(), "a"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// sealed makes a record that the ring of the given key ID sealed.
func sealed(t testing.TB, name string, id string, seed byte, value string) secrets.Record {
	t.Helper()
	env, err := secrets.Seal(testRing(t, id, seed), []byte(value), secrets.DataAAD(name))
	if err != nil {
		t.Fatal(err)
	}
	return secrets.Record{Name: name, Envelope: env}
}

// U15
func TestRewrapCounts(t *testing.T) {
	recs := []secrets.Record{sealed(t, "a", "k0", 9, "x"), sealed(t, "b", "k0", 9, "y"), sealed(t, "c", "k0", 9, "z")}
	ring := testRing(t, "k1", 1, prevKey{"k0", 9})
	repo := &mock.SecretsRepository{
		ListForRewrapFunc: func(_ context.Context, p secrets.RewrapListParams) ([]secrets.Record, error) {
			if p.After == "" {
				return recs, nil
			}
			return nil, nil
		},
		UpdateEnvelopeFunc: func(_ context.Context, p secrets.UpdateEnvelopeParams) error {
			if p.Name == "b" {
				return secrets.ErrConflict
			}
			return nil
		},
	}
	res, err := secrets.NewService(ring, repo).Rewrap(context.Background(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if res != (secrets.RewrapResult{Rewrapped: 2, Conflicts: 1, Failed: 0}) {
		t.Fatalf("result = %+v", res)
	}
	params := repo.ListForRewrapParams()
	if len(params) != 2 || params[1].After != "c" || params[0].NotKeyID != "k1" {
		t.Fatalf("params = %+v", params)
	}
}

// logBuffer replaces the default logger and returns the buffer.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *logBuffer {
	t.Helper()
	b := &logBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return b
}

// U16
func TestNoPlaintextInSpansOrLogs(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old); _ = tp.Shutdown(context.Background()) })
	logs := captureLogs(t)

	repo := &mock.SecretsRepository{}
	svc := newService(t, repo)
	plain := "PLAINTEXT-MARKER-42"
	ctx := context.Background()
	if err := svc.Put(ctx, "a", []byte(plain)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	// A failed Get with a wrong key also leaves no value.
	repo.Put("b", sealed(t, "b", "k0", 9, plain).Envelope)
	_, _ = svc.Get(ctx, "b")
	_, _ = svc.Rewrap(ctx, 10)

	spans := exp.GetSpans()
	if len(spans) < 4 {
		t.Fatalf("got %d spans", len(spans))
	}
	var seen bool
	for _, s := range spans {
		for _, a := range s.Attributes {
			if strings.Contains(a.Value.String(), plain) {
				t.Fatalf("span %s attribute %s contains the plaintext", s.Name, a.Key)
			}
			if a.Key == "secret.name" {
				seen = true
			}
		}
		if strings.Contains(s.Status.Description, plain) {
			t.Fatal("a span status contains the plaintext")
		}
	}
	if !seen {
		t.Fatal("no span has secret.name")
	}
	if strings.Contains(logs.String(), plain) {
		t.Fatal("a log record contains the plaintext")
	}
}

// U29
func TestRewrapFailedRecord(t *testing.T) {
	logs := captureLogs(t)
	recs := []secrets.Record{sealed(t, "a", "k0", 9, "x"), sealed(t, "b", "k2", 2, "y")}
	// The ring has k1 (current) and k2. It does not have k0.
	ring := testRing(t, "k1", 1, prevKey{"k2", 2})
	repo := &mock.SecretsRepository{ListForRewrapFunc: func(_ context.Context, p secrets.RewrapListParams) ([]secrets.Record, error) {
		if p.After == "" {
			return recs, nil
		}
		return nil, nil
	}, UpdateEnvelopeFunc: func(context.Context, secrets.UpdateEnvelopeParams) error { return nil }}
	res, err := secrets.NewService(ring, repo).Rewrap(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if res != (secrets.RewrapResult{Rewrapped: 1, Failed: 1}) {
		t.Fatalf("result = %+v", res)
	}
	out := logs.String()
	if strings.Count(out, `"level":"ERROR"`) != 1 || !strings.Contains(out, `"secret.name":"a"`) {
		t.Fatalf("log = %s", out)
	}
	for _, bad := range []string{testKey(9), testKey(1), testKey(2)} {
		if strings.Contains(out, bad) {
			t.Fatal("the log contains a key")
		}
	}
}

// U30
func TestRewrapAlwaysStops(t *testing.T) {
	names := []string{"a", "b"}
	repo := &mock.SecretsRepository{
		ListForRewrapFunc: func(_ context.Context, p secrets.RewrapListParams) ([]secrets.Record, error) {
			for _, n := range names {
				if n > p.After {
					return []secrets.Record{sealed(t, n, "k0", 9, "x")}, nil
				}
			}
			return nil, nil
		},
		UpdateEnvelopeFunc: func(context.Context, secrets.UpdateEnvelopeParams) error { return secrets.ErrConflict },
	}
	svc := secrets.NewService(testRing(t, "k1", 1, prevKey{"k0", 9}), repo)
	res, err := svc.Rewrap(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Conflicts != 2 || repo.ListForRewrapCalls() != 3 {
		t.Fatalf("result = %+v, list calls = %d", res, repo.ListForRewrapCalls())
	}
}

// U31
func TestRewrapBatchSize(t *testing.T) {
	repo := &mock.SecretsRepository{}
	svc := newService(t, repo)
	for _, n := range []int{0, 1001, -1} {
		if _, err := svc.Rewrap(context.Background(), n); !errors.Is(err, secrets.ErrInvalidBatchSize) {
			t.Fatalf("batch %d: err = %v", n, err)
		}
	}
	if repo.ListForRewrapCalls() != 0 {
		t.Fatal("the repository was called")
	}
}

func TestRewrapRepositoryError(t *testing.T) {
	boom := errors.New("boom")
	repo := &mock.SecretsRepository{
		ListForRewrapFunc: func(context.Context, secrets.RewrapListParams) ([]secrets.Record, error) {
			return []secrets.Record{sealed(t, "a", "k0", 9, "x")}, nil
		},
		UpdateEnvelopeFunc: func(context.Context, secrets.UpdateEnvelopeParams) error { return boom },
	}
	svc := secrets.NewService(testRing(t, "k1", 1, prevKey{"k0", 9}), repo)
	res, err := svc.Rewrap(context.Background(), 5)
	if !errors.Is(err, boom) || res != (secrets.RewrapResult{}) {
		t.Fatalf("result %+v, err %v", res, err)
	}
}

func TestRewrapContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newService(t, &mock.SecretsRepository{}).Rewrap(ctx, 5); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// U34
func TestListLimit(t *testing.T) {
	repo := &mock.SecretsRepository{}
	svc := newService(t, repo)
	ctx := context.Background()
	for _, n := range []int{-1, 1001} {
		if _, err := svc.List(ctx, secrets.ListParams{Limit: n}); !errors.Is(err, secrets.ErrInvalidLimit) {
			t.Fatalf("limit %d: err = %v", n, err)
		}
	}
	for _, n := range []int{0, 1000} {
		if _, err := svc.List(ctx, secrets.ListParams{Limit: n}); err != nil {
			t.Fatal(err)
		}
	}
	got := repo.ListNamesParams()
	if len(got) != 2 || got[0].Limit != 100 || got[1].Limit != 1000 {
		t.Fatalf("limits = %+v", got)
	}
}

func TestDeleteAndNameChecks(t *testing.T) {
	repo := &mock.SecretsRepository{}
	svc := newService(t, repo)
	ctx := context.Background()
	if err := svc.Delete(ctx, "A"); !errors.Is(err, secrets.ErrInvalidName) {
		t.Fatalf("err = %v", err)
	}
	if _, err := svc.Get(ctx, "A"); !errors.Is(err, secrets.ErrInvalidName) {
		t.Fatalf("err = %v", err)
	}
	if repo.DeleteCalls() != 0 || repo.GetCalls() != 0 {
		t.Fatal("the repository was called")
	}
	if err := svc.Delete(ctx, "a"); !errors.Is(err, secrets.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}
