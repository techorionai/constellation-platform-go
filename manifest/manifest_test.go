package manifest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/techorionai/constellation-platform-go/manifest"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

var testManifest = manifest.PermissionManifest{
	App: "helpdesk",
	Permissions: []manifest.PermissionDefinition{
		{Key: "helpdesk:ticket:read", Description: "Read tickets."},
	},
	DefaultRoles: []manifest.RoleDefinition{
		{Key: "agent", Name: "Agent", Description: "An agent.", Permissions: []string{"helpdesk:ticket:read"}},
	},
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type harness struct {
	pub    *manifest.Publisher
	logs   *syncBuffer
	reader *sdkmetric.ManualReader
}

func newHarness(reg manifest.Registry) *harness {
	logs := &syncBuffer{}
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	pub := manifest.NewPublisher(reg, testManifest,
		manifest.WithLogger(log), manifest.WithMeterProvider(mp),
		manifest.WithBackoff(time.Millisecond, 4*time.Millisecond))
	return &harness{pub: pub, logs: logs, reader: reader}
}

func (h *harness) outcomes(t *testing.T) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "constellation.manifest.publish" {
				continue
			}
			if m.Unit != "{attempt}" {
				t.Errorf("unit = %q", m.Unit)
			}
			for _, dp := range m.Data.(metricdata.Sum[int64]).DataPoints {
				v, _ := dp.Attributes.Value(attribute.Key("outcome"))
				out[v.AsString()] += dp.Value
			}
		}
	}
	return out
}

func count(logs, level string) int { return strings.Count(logs, "level="+level+" ") }

// U11: two unavailable results, then success.
func TestU11RetryUnavailable(t *testing.T) {
	var n int
	reg := &mock.ManifestRegistry{PublishFunc: func(context.Context, manifest.PermissionManifest) error {
		n++
		if n <= 2 {
			return manifest.ErrUnavailable
		}
		return nil
	}}
	h := newHarness(reg)
	chk := h.pub.Checker()
	if chk.Name() != "manifest" {
		t.Errorf("name = %q", chk.Name())
	}
	if err := chk.Check(context.Background()); err == nil {
		t.Fatal("the checker is ready before the publish")
	}
	if err := h.pub.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reg.PublishCalls() != 3 {
		t.Errorf("attempts = %d", reg.PublishCalls())
	}
	if err := chk.Check(context.Background()); err != nil {
		t.Errorf("checker = %v", err)
	}
	logs := h.logs.String()
	if count(logs, "WARN") != 2 || count(logs, "INFO") != 1 {
		t.Errorf("logs:\n%s", logs)
	}
	if got := h.outcomes(t); got["unavailable"] != 2 || got["ok"] != 1 {
		t.Errorf("outcomes = %v", got)
	}
}

// U12: invalid manifest, no retry.
func TestU12Invalid(t *testing.T) {
	reg := &mock.ManifestRegistry{PublishFunc: func(context.Context, manifest.PermissionManifest) error {
		return &manifest.InvalidError{Code: "unprocessable", Problems: []manifest.Problem{{Code: "permission_app_mismatch", Field: "/permissions/0/key"}}}
	}}
	h := newHarness(reg)
	err := h.pub.Run(context.Background())
	var inv *manifest.InvalidError
	if !errors.As(err, &inv) {
		t.Fatalf("err = %v", err)
	}
	if reg.PublishCalls() != 1 {
		t.Errorf("attempts = %d", reg.PublishCalls())
	}
	if h.pub.Checker().Check(context.Background()) == nil {
		t.Error("the checker is ready")
	}
	logs := h.logs.String()
	if count(logs, "ERROR") != 1 || !strings.Contains(logs, "permission_app_mismatch") {
		t.Errorf("logs:\n%s", logs)
	}
	if got := h.outcomes(t); got["invalid"] != 1 {
		t.Errorf("outcomes = %v", got)
	}
}

// U12a: denied, then success.
func TestU12aDenied(t *testing.T) {
	var n int
	reg := &mock.ManifestRegistry{PublishFunc: func(context.Context, manifest.PermissionManifest) error {
		n++
		if n == 1 {
			return manifest.ErrDenied
		}
		return nil
	}}
	h := newHarness(reg)
	if err := h.pub.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reg.PublishCalls() != 2 {
		t.Errorf("attempts = %d", reg.PublishCalls())
	}
	if count(h.logs.String(), "ERROR") != 1 {
		t.Errorf("logs:\n%s", h.logs.String())
	}
	if got := h.outcomes(t); got["denied"] != 1 || got["ok"] != 1 {
		t.Errorf("outcomes = %v", got)
	}
}

// An error of another kind counts as unavailable.
func TestUnknownErrorRetries(t *testing.T) {
	var n int
	reg := &mock.ManifestRegistry{PublishFunc: func(context.Context, manifest.PermissionManifest) error {
		n++
		if n == 1 {
			return errors.New("boom")
		}
		return nil
	}}
	h := newHarness(reg)
	if err := h.pub.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := h.outcomes(t); got["unavailable"] != 1 {
		t.Errorf("outcomes = %v", got)
	}
}

// U13: the context ends.
func TestU13ContextEnds(t *testing.T) {
	reg := &mock.ManifestRegistry{PublishFunc: func(context.Context, manifest.PermissionManifest) error {
		return manifest.ErrUnavailable
	}}
	h := newHarness(reg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.pub.Run(ctx) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return")
	}
	if h.pub.Checker().Check(context.Background()) == nil {
		t.Error("the checker is ready")
	}
}

const validJSON = `{
  "app": "helpdesk",
  "permissions": [
    {"key": "helpdesk:ticket:read", "description": "Read."},
    {"key": "helpdesk:ticket:write", "description": "Write."}
  ],
  "defaultRoles": [
    {"key": "agent", "name": "Agent", "description": "A.", "permissions": ["helpdesk:ticket:read"]}
  ]
}`

// U14: Load.
func TestU14Load(t *testing.T) {
	load := func(content string) error {
		_, err := manifest.Load(fstest.MapFS{"manifest.json": {Data: []byte(content)}}, "manifest.json")
		return err
	}
	m, err := manifest.Load(fstest.MapFS{"manifest.json": {Data: []byte(validJSON)}}, "manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if m.App != "helpdesk" || len(m.Permissions) != 2 || len(m.DefaultRoles) != 1 {
		t.Errorf("manifest = %+v", m)
	}
	cases := map[string]struct{ old, new, want string }{
		"no app prefix":      {`"helpdesk:ticket:write"`, `"other:ticket:write"`, "app prefix"},
		"bad key shape":      {`"helpdesk:ticket:write"`, `"helpdesk:write"`, "valid permission key"},
		"unknown role perm":  {`["helpdesk:ticket:read"]`, `["helpdesk:ticket:delete"]`, "not in the manifest"},
		"duplicate key":      {`"helpdesk:ticket:write"`, `"helpdesk:ticket:read"`, "duplicate key"},
		"bad app":            {`"app": "helpdesk"`, `"app": "Help Desk"`, "app key"},
		"unknown JSON field": {`"app": "helpdesk"`, `"app": "helpdesk", "extra": 1`, "extra"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := load(strings.Replace(validJSON, tc.old, tc.new, 1))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := manifest.Load(fstest.MapFS{}, "missing.json"); err == nil {
		t.Error("no error for a missing file")
	}
	for _, tail := range []string{"}", "]", "{}"} {
		if err := load(validJSON + tail); err == nil {
			t.Errorf("no error for trailing %q", tail)
		}
	}
	if err := load("{"); err == nil {
		t.Error("no error for bad JSON")
	}
}

func TestLoadIdentityAdmin(t *testing.T) {
	m := manifest.PermissionManifest{
		App:         "identity",
		Permissions: []manifest.PermissionDefinition{{Key: "identity:admin"}, {Key: "identity:user:read"}},
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.App = "other"
	m.Permissions = []manifest.PermissionDefinition{{Key: "identity:admin"}}
	if err := m.Validate(); err == nil {
		t.Error("identity:admin is valid for the app other")
	}
}

func TestInvalidErrorText(t *testing.T) {
	e := &manifest.InvalidError{Code: "x", Problems: []manifest.Problem{{Code: "a", Field: "/f"}}}
	if !strings.Contains(e.Error(), "x") || !strings.Contains(e.Error(), "a /f") {
		t.Errorf("text = %q", e.Error())
	}
	if len(e.Codes()) != 2 {
		t.Errorf("codes = %v", e.Codes())
	}
}

// U12b: rejected, no retry.
func TestU12bRejected(t *testing.T) {
	reg := &mock.ManifestRegistry{PublishFunc: func(context.Context, manifest.PermissionManifest) error {
		return fmt.Errorf("%w: status 409", manifest.ErrRejected)
	}}
	h := newHarness(reg)
	err := h.pub.Run(context.Background())
	if !errors.Is(err, manifest.ErrRejected) {
		t.Fatalf("err = %v", err)
	}
	if reg.PublishCalls() != 1 {
		t.Errorf("attempts = %d", reg.PublishCalls())
	}
	if h.pub.Checker().Check(context.Background()) == nil {
		t.Error("the checker is ready")
	}
	logs := h.logs.String()
	if count(logs, "ERROR") != 1 || !strings.Contains(logs, "status 409") {
		t.Errorf("logs:\n%s", logs)
	}
	if got := h.outcomes(t); got["rejected"] != 1 || len(got) != 1 {
		t.Errorf("outcomes = %v", got)
	}
}

// R9: a success at the end of the context counts as a success.
func TestSuccessWithEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reg := &mock.ManifestRegistry{PublishFunc: func(context.Context, manifest.PermissionManifest) error {
		cancel()
		return nil
	}}
	h := newHarness(reg)
	if err := h.pub.Run(ctx); err != nil {
		t.Fatalf("err = %v", err)
	}
	if !h.pub.Published() || h.pub.Checker().Check(context.Background()) != nil {
		t.Error("the checker is not ready after a success")
	}
	if got := h.outcomes(t); got["ok"] != 1 {
		t.Errorf("outcomes = %v", got)
	}
}

// R10: the retry logs have the error and the real wait.
func TestRetryLogs(t *testing.T) {
	var n int
	reg := &mock.ManifestRegistry{PublishFunc: func(context.Context, manifest.PermissionManifest) error {
		n++
		switch n {
		case 1:
			return fmt.Errorf("%w: status 503", manifest.ErrUnavailable)
		case 2:
			return fmt.Errorf("%w: status 401", manifest.ErrDenied)
		}
		return nil
	}}
	h := newHarness(reg)
	if err := h.pub.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	logs := h.logs.String()
	for _, want := range []string{"status 503", "status 401"} {
		if !strings.Contains(logs, want) {
			t.Errorf("logs have no %q:\n%s", want, logs)
		}
	}
	// The backoff is 1 ms to 4 ms with jitter. The first wait is 0.5 ms to 1 ms.
	// The base wait of the first retry is 1ms, so a jittered wait is below it.
	if strings.Contains(logs, "wait=1ms") {
		t.Errorf("the log shows the base wait:\n%s", logs)
	}
	if strings.Count(logs, "wait=") != 2 {
		t.Errorf("logs:\n%s", logs)
	}
}

// R17: a manifest that is not valid is not sent.
func TestInvalidManifestNotSent(t *testing.T) {
	reg := &mock.ManifestRegistry{}
	m := testManifest
	m.App = ".."
	pub := manifest.NewPublisher(reg, m, manifest.WithLogger(slog.New(slog.NewTextHandler(&syncBuffer{}, nil))),
		manifest.WithMeterProvider(sdkmetric.NewMeterProvider()))
	err := pub.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "invalid manifest") {
		t.Fatalf("err = %v", err)
	}
	if reg.PublishCalls() != 0 {
		t.Errorf("the registry got %d requests", reg.PublishCalls())
	}
	if pub.Checker().Check(context.Background()) == nil {
		t.Error("the checker is ready")
	}
}
