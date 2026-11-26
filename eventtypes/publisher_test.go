package eventtypes_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/techorionai/constellation-platform-go/eventtypes"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

const marker = "SECRET-MARKER-TEXT"

func markedReg() eventtypes.Registration {
	r := goodReg()
	r.EventTypes = append(r.EventTypes, goodType("helpdesk.ticket.closed"))
	for i := range r.EventTypes {
		r.EventTypes[i].Description = "Description " + marker
		r.EventTypes[i].DataSchema = json.RawMessage(`{"type":"object","description":"` + marker + `"}`)
	}
	return r
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
	pub    *eventtypes.Publisher
	logs   *syncBuffer
	reader *sdkmetric.ManualReader
	waits  []time.Duration
}

func newHarness(reg eventtypes.Registry, r eventtypes.Registration) *harness {
	h := &harness{logs: &syncBuffer{}, reader: sdkmetric.NewManualReader()}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader))
	log := slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h.pub = eventtypes.NewPublisher(reg, r,
		eventtypes.WithLogger(log), eventtypes.WithMeterProvider(mp),
		eventtypes.WithBackoff(time.Second, time.Minute))
	eventtypes.SetSleep(h.pub, func(ctx context.Context, d time.Duration) error {
		h.waits = append(h.waits, d)
		return ctx.Err()
	})
	return h
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
			if m.Name != "constellation.events.register" {
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

func levelCount(logs, level string) int { return strings.Count(logs, "level="+level+" ") }

func ready(h *harness) bool { return h.pub.Checker().Check(context.Background()) == nil }

// sequence returns a registry that gives the errors in order, then success.
func sequence(res eventtypes.Result, errs ...error) *mock.EventTypeRegistry {
	var n int
	return &mock.EventTypeRegistry{RegisterFunc: func(context.Context, eventtypes.Registration) (eventtypes.Result, error) {
		n++
		if n <= len(errs) {
			return eventtypes.Result{}, errs[n-1]
		}
		return res, nil
	}}
}

var okResult = eventtypes.Result{App: "helpdesk", Version: 3, EventTypeCount: 2}

// Issue 22 U5
func TestU5RetryUnavailable(t *testing.T) {
	unavailable := fmt.Errorf("%w: status 503", eventtypes.ErrUnavailable)
	reg := sequence(okResult, unavailable, unavailable)
	h := newHarness(reg, markedReg())
	if ready(h) {
		t.Fatal("ready before")
	}
	if err := h.pub.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reg.RegisterCalls() != 3 || len(h.waits) != 2 || !ready(h) || !h.pub.Registered() {
		t.Fatalf("calls %d waits %v ready %v", reg.RegisterCalls(), h.waits, ready(h))
	}
	// Backoff 1 s then 2 s, with jitter from half to full.
	if h.waits[0] < 500*time.Millisecond || h.waits[0] > time.Second || h.waits[1] < time.Second || h.waits[1] > 2*time.Second {
		t.Errorf("waits %v", h.waits)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "version=3") || !strings.Contains(logs, "eventTypeCount=2") {
		t.Errorf("logs:\n%s", logs)
	}
	if o := h.outcomes(t); o["unavailable"] != 2 || o["ok"] != 1 {
		t.Errorf("outcomes %v", o)
	}
	if h.pub.Checker().Name() != "event_types" {
		t.Errorf("checker name %q", h.pub.Checker().Name())
	}
}

// Issue 22 U6
func TestU6RetryAfter(t *testing.T) {
	reg := sequence(okResult,
		&eventtypes.UnavailableError{RetryAfter: 30 * time.Second},
		&eventtypes.UnavailableError{RetryAfter: 10 * time.Minute})
	h := newHarness(reg, markedReg())
	if err := h.pub.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.waits) != 2 || h.waits[0] != 30*time.Second || h.waits[1] != 5*time.Minute {
		t.Errorf("waits %v", h.waits)
	}
}

// Issue 22 U7
func TestU7Invalid(t *testing.T) {
	for _, code := range []string{"registration_app_mismatch", "content_too_large"} {
		reg := sequence(okResult, &eventtypes.InvalidError{Code: code})
		h := newHarness(reg, markedReg())
		err := h.pub.Run(context.Background())
		var inv *eventtypes.InvalidError
		if !errors.As(err, &inv) || inv.Code != code {
			t.Fatalf("err = %v", err)
		}
		if reg.RegisterCalls() != 1 || ready(h) || len(h.waits) != 0 {
			t.Errorf("calls %d ready %v waits %v", reg.RegisterCalls(), ready(h), h.waits)
		}
		logs := h.logs.String()
		if levelCount(logs, "ERROR") != 1 || !strings.Contains(logs, code) {
			t.Errorf("logs:\n%s", logs)
		}
		if o := h.outcomes(t); o["invalid"] != 1 {
			t.Errorf("outcomes %v", o)
		}
	}
}

// Issue 22 U8
func TestU8Rejected(t *testing.T) {
	reg := sequence(okResult, fmt.Errorf("%w: status 409", eventtypes.ErrRejected))
	h := newHarness(reg, markedReg())
	err := h.pub.Run(context.Background())
	if !errors.Is(err, eventtypes.ErrRejected) {
		t.Fatalf("err = %v", err)
	}
	if reg.RegisterCalls() != 1 || ready(h) || levelCount(h.logs.String(), "ERROR") != 1 {
		t.Errorf("calls %d ready %v", reg.RegisterCalls(), ready(h))
	}
	if o := h.outcomes(t); o["rejected"] != 1 {
		t.Errorf("outcomes %v", o)
	}
}

// Issue 22 U9
func TestU9DeniedRetries(t *testing.T) {
	denied := fmt.Errorf("%w: status 403", eventtypes.ErrDenied)
	reg := sequence(okResult, denied, denied)
	h := newHarness(reg, markedReg())
	if err := h.pub.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if reg.RegisterCalls() != 3 || !ready(h) || len(h.waits) != 2 {
		t.Errorf("calls %d ready %v waits %v", reg.RegisterCalls(), ready(h), h.waits)
	}
	if n := levelCount(h.logs.String(), "ERROR"); n != 2 {
		t.Errorf("%d error logs", n)
	}
	if o := h.outcomes(t); o["denied"] != 2 || o["ok"] != 1 {
		t.Errorf("outcomes %v", o)
	}
}

// Issue 22 U10
func TestU10ContextAndInvalidRegistration(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reg := sequence(okResult, eventtypes.ErrUnavailable, eventtypes.ErrUnavailable)
	h := newHarness(reg, markedReg())
	eventtypes.SetSleep(h.pub, func(ctx context.Context, d time.Duration) error {
		cancel()
		return ctx.Err()
	})
	if err := h.pub.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if ready(h) {
		t.Error("ready")
	}

	bad := markedReg()
	bad.Source = "/other"
	reg2 := &mock.EventTypeRegistry{}
	h2 := newHarness(reg2, bad)
	err := h2.pub.Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "/source") {
		t.Fatalf("err = %v", err)
	}
	if reg2.RegisterCalls() != 0 || ready(h2) {
		t.Errorf("calls %d ready %v", reg2.RegisterCalls(), ready(h2))
	}
	if o := h2.outcomes(t); o["invalid"] != 1 {
		t.Errorf("outcomes %v", o)
	}
}

// Issue 22 U11
func TestU11CountDiffers(t *testing.T) {
	reg := sequence(eventtypes.Result{App: "helpdesk", Version: 1, EventTypeCount: 1})
	h := newHarness(reg, markedReg())
	if err := h.pub.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	logs := h.logs.String()
	if !ready(h) || levelCount(logs, "WARN") != 1 || !strings.Contains(logs, "fileEventTypeCount=2") {
		t.Errorf("ready %v logs:\n%s", ready(h), logs)
	}
}

// Issue 22 U12: no log record has a schema or a description.
func TestU12NoSchemaInLogs(t *testing.T) {
	cases := []error{
		fmt.Errorf("%w: status 503", eventtypes.ErrUnavailable),
		fmt.Errorf("%w: status 403", eventtypes.ErrDenied),
		&eventtypes.InvalidError{Code: "invalid_data_schema", Problems: []eventtypes.Problem{{Code: "x", Field: "/eventTypes/0/dataSchema"}}},
		fmt.Errorf("%w: status 409", eventtypes.ErrRejected),
	}
	for _, e := range cases {
		reg := sequence(okResult, e)
		h := newHarness(reg, markedReg())
		_ = h.pub.Run(context.Background())
		if strings.Contains(h.logs.String(), marker) {
			t.Errorf("logs have the marker:\n%s", h.logs.String())
		}
	}
	h := newHarness(sequence(okResult, eventtypes.ErrUnavailable), markedReg())
	_ = h.pub.Run(context.Background())
	if strings.Contains(h.logs.String(), marker) {
		t.Errorf("logs have the marker:\n%s", h.logs.String())
	}
	bad := markedReg()
	bad.Source = "/other"
	h = newHarness(&mock.EventTypeRegistry{}, bad)
	_ = h.pub.Run(context.Background())
	if strings.Contains(h.logs.String(), marker) {
		t.Errorf("logs have the marker:\n%s", h.logs.String())
	}
}

func TestInvalidErrorText(t *testing.T) {
	e := &eventtypes.InvalidError{Code: "a", Problems: []eventtypes.Problem{{Code: "b", Field: "/x"}}}
	if got := e.Error(); !strings.Contains(got, "a") || !strings.Contains(got, "[b /x]") {
		t.Errorf("text %q", got)
	}
	if c := e.Codes(); len(c) != 2 || c[0] != "a" || c[1] != "b" {
		t.Errorf("codes %v", c)
	}
	u := &eventtypes.UnavailableError{Cause: errors.New("boom"), RetryAfter: time.Second}
	if !errors.Is(u, eventtypes.ErrUnavailable) || !strings.Contains(u.Error(), "boom") {
		t.Errorf("unavailable %v", u)
	}
}
