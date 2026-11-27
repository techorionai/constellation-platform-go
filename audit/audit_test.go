package audit_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/audit"
	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/clientinfo"
	"github.com/techorionai/constellation-platform-go/requestid"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

const (
	app      = "helpdesk"
	clientID = "helpdesk-client"
)

func newWriter(t *testing.T) (*audit.EntryWriter, *mock.AuditInserter) {
	t.Helper()
	ins := &mock.AuditInserter{}
	w, err := audit.New(ins, audit.Config{App: app, ClientID: clientID})
	if err != nil {
		t.Fatal(err)
	}
	return w, ins
}

func okEntry() audit.Entry {
	return audit.Entry{Action: "helpdesk:ticket:write", Result: audit.Success}
}

type decoded struct {
	SpecVersion     string         `json:"specversion"`
	ID              string         `json:"id"`
	Source          string         `json:"source"`
	Type            string         `json:"type"`
	Time            string         `json:"time"`
	DataContentType string         `json:"datacontenttype"`
	Actor           string         `json:"actor"`
	ActorType       string         `json:"actortype"`
	Act             string         `json:"act"`
	ActType         string         `json:"acttype"`
	Traceparent     string         `json:"traceparent"`
	Data            map[string]any `json:"data"`
}

func decode(t *testing.T, payload []byte) decoded {
	t.Helper()
	var d decoded
	if err := json.Unmarshal(payload, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

// U1: no transaction.
func TestU1NoTransaction(t *testing.T) {
	ins := &mock.AuditInserter{NoTx: true}
	w, err := audit.New(ins, audit.Config{App: app, ClientID: clientID})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Write(context.Background(), okEntry()); !errors.Is(err, audit.ErrNoTransaction) {
		t.Errorf("err = %v, want ErrNoTransaction", err)
	}
	if ins.InsertCalls() != 0 {
		t.Error("Insert was called")
	}
}

// U2: the action rules.
func TestU2Action(t *testing.T) {
	w, ins := newWriter(t)
	ctx := context.Background()
	if err := w.Write(ctx, okEntry()); err != nil {
		t.Fatalf("valid action: %v", err)
	}
	for _, a := range []string{"crm:ticket:write", "helpdesk:ticket", ""} {
		e := okEntry()
		e.Action = a
		if err := w.Write(ctx, e); !errors.Is(err, audit.ErrInvalidAction) {
			t.Errorf("action %q: err = %v, want ErrInvalidAction", a, err)
		}
	}
	if n := len(ins.Rows()); n != 1 {
		t.Errorf("rows = %d, want 1", n)
	}
}

// U3: the rules of each field.
func TestU3Rules(t *testing.T) {
	big := map[string]any{"k": strings.Repeat("a", 16<<10)}
	cases := []struct {
		name string
		mod  func(*audit.Entry)
		want error
	}{
		{"result ok", func(e *audit.Entry) { e.Result = "ok" }, audit.ErrInvalidResult},
		{"empty result", func(e *audit.Entry) { e.Result = "" }, audit.ErrInvalidResult},
		{"empty target id", func(e *audit.Entry) { e.Target = &audit.Target{Type: "ticket"} }, audit.ErrInvalidTarget},
		{"long target id", func(e *audit.Entry) { e.Target = &audit.Target{Type: "ticket", ID: strings.Repeat("a", 257)} }, audit.ErrInvalidTarget},
		{"bad target type", func(e *audit.Entry) { e.Target = &audit.Target{Type: "Bad-Type", ID: "1"} }, audit.ErrInvalidTarget},
		{"long summary", func(e *audit.Entry) { e.Summary = strings.Repeat("a", 1025) }, audit.ErrInvalidSummary},
		{"big details", func(e *audit.Entry) { e.Details = big }, audit.ErrInvalidDetails},
		{"unmarshalable details", func(e *audit.Entry) { e.Details = map[string]any{"c": make(chan int)} }, audit.ErrInvalidDetails},
		{"NUL in summary", func(e *audit.Entry) { e.Summary = "a\x00b" }, audit.ErrInvalidText},
		{"NUL in detail", func(e *audit.Entry) { e.Details = map[string]any{"k": "a\x00b"} }, audit.ErrInvalidText},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, ins := newWriter(t)
			e := okEntry()
			c.mod(&e)
			if err := w.Write(context.Background(), e); !errors.Is(err, c.want) {
				t.Errorf("err = %v, want %v", err, c.want)
			}
			if ins.InsertCalls() != 0 {
				t.Error("Insert was called")
			}
		})
	}
	// The limits themselves are valid.
	w, _ := newWriter(t)
	e := okEntry()
	e.Summary = strings.Repeat("a", 1024)
	e.Target = &audit.Target{Type: "ticket", ID: strings.Repeat("a", 256)}
	e.Details = map[string]any{"k": strings.Repeat("a", 16<<10-8)}
	if err := w.Write(context.Background(), e); err != nil {
		t.Errorf("entry at the limits: %v", err)
	}
}

// U4: the actor.
func TestU4Actor(t *testing.T) {
	w, ins := newWriter(t)
	p := auth.Principal{ID: "user-1", Type: auth.PrincipalUser, Act: &auth.Actor{ID: "agent-1", Type: auth.PrincipalAgent}}
	ctx := auth.ContextWithPrincipal(context.Background(), p)
	if err := w.Write(ctx, okEntry()); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(context.Background(), okEntry()); err != nil {
		t.Fatal(err)
	}
	rows := ins.Rows()
	d1, d2 := decode(t, rows[0].Payload), decode(t, rows[1].Payload)
	if d1.Actor != "user-1" || d1.ActorType != string(auth.PrincipalUser) || d1.Act != "agent-1" || d1.ActType != string(auth.PrincipalAgent) {
		t.Errorf("principal actor = %+v", d1)
	}
	if d2.Actor != clientID || d2.ActorType != string(auth.PrincipalService) || d2.Act != "" {
		t.Errorf("service actor = %+v", d2)
	}
}

func tracedContext(t *testing.T) (context.Context, *tracetest.SpanRecorder) {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, _ := tp.Tracer("test").Start(context.Background(), "op")
	return ctx, rec
}

// U5: the CloudEvent of a full entry.
func TestU5CloudEvent(t *testing.T) {
	w, ins := newWriter(t)
	ctx, _ := tracedContext(t)
	ctx = requestid.ContextWithID(ctx, "req-1")
	ctx = clientinfo.ContextWith(ctx, clientinfo.Info{IP: netip.MustParseAddr("203.0.113.9"), UserAgent: "agent/1.0"})
	e := audit.Entry{
		Action: "helpdesk:ticket:write", Result: audit.Denied,
		Target: &audit.Target{Type: "ticket", ID: "T-1"}, Summary: "no access",
		Details: map[string]any{"reason": "scope"},
	}
	if err := w.Write(ctx, e); err != nil {
		t.Fatal(err)
	}
	row := ins.Rows()[0]
	d := decode(t, row.Payload)
	if d.SpecVersion != "1.0" || d.Source != "/helpdesk" || d.Type != "helpdesk.audit.recorded" || d.DataContentType != "application/json" {
		t.Errorf("envelope = %+v", d)
	}
	if id, err := uuid.Parse(d.ID); err != nil || id.Version() != 7 || row.EventID != d.ID || row.Type != d.Type {
		t.Errorf("id %q, row %+v", d.ID, row)
	}
	if !strings.HasPrefix(d.Traceparent, "00-") || len(d.Traceparent) != 55 {
		t.Errorf("traceparent = %q", d.Traceparent)
	}
	want := map[string]any{
		"action": "helpdesk:ticket:write", "result": "denied", "summary": "no access",
		"target": map[string]any{"type": "ticket", "id": "T-1"}, "details": map[string]any{"reason": "scope"},
		"requestId": "req-1", "ipAddress": "203.0.113.9", "userAgent": "agent/1.0",
	}
	wantJSON, _ := json.Marshal(want)
	gotJSON, _ := json.Marshal(d.Data)
	if !bytes.Equal(wantJSON, gotJSON) {
		t.Errorf("data = %s\nwant %s", gotJSON, wantJSON)
	}
}

// U7: the configuration.
func TestU7Config(t *testing.T) {
	ins := &mock.AuditInserter{}
	for field, cfg := range map[string]audit.Config{
		"App":      {App: "Bad", ClientID: "c"},
		"ClientID": {App: app},
	} {
		if _, err := audit.New(ins, cfg); !errors.Is(err, audit.ErrInvalidConfig) || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: err = %v", field, err)
		}
	}
	if _, err := audit.New(nil, audit.Config{App: app, ClientID: "c"}); !errors.Is(err, audit.ErrInvalidConfig) {
		t.Errorf("nil inserter: err = %v", err)
	}
}

// U8: no log record and no entry data in spans.
func TestU8NoLogNoSpanData(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })

	w, _ := newWriter(t)
	ctx, rec := tracedContext(t)
	ctx = clientinfo.ContextWith(ctx, clientinfo.Info{IP: netip.MustParseAddr("203.0.113.9"), UserAgent: "ua"})
	if err := w.Write(ctx, audit.Entry{Action: "helpdesk:ticket:write", Result: audit.Success, Summary: "secret-summary"}); err != nil {
		t.Fatal(err)
	}
	if logs.Len() != 0 {
		t.Errorf("log output: %s", logs.String())
	}
	trace.SpanFromContext(ctx).End()
	for _, s := range rec.Ended() {
		for _, kv := range s.Attributes() {
			if v := kv.Value.String(); strings.Contains(v, "secret-summary") || strings.Contains(v, "203.0.113.9") {
				t.Errorf("span attribute %s has entry data", kv.Key)
			}
		}
		if len(s.Events()) != 0 {
			t.Error("span has events")
		}
	}
}

// U17: bad context values are left out.
func TestU17ContextValues(t *testing.T) {
	cases := []struct {
		name    string
		reqID   string
		info    clientinfo.Info
		missing []string
		ip      string
	}{
		{"long request id", strings.Repeat("a", 129), clientinfo.Info{}, []string{"requestId"}, ""},
		{"space in request id", "a b", clientinfo.Info{}, []string{"requestId"}, ""},
		{"empty user agent", "", clientinfo.Info{UserAgent: ""}, []string{"userAgent", "requestId"}, ""},
		{"long user agent", "", clientinfo.Info{UserAgent: strings.Repeat("a", 513)}, []string{"userAgent"}, ""},
		{"NUL user agent", "", clientinfo.Info{UserAgent: "a\x00b"}, []string{"userAgent"}, ""},
		{"bad utf8 user agent", "", clientinfo.Info{UserAgent: "a\xffb"}, []string{"userAgent"}, ""},
		{"zone in ip", "", clientinfo.Info{IP: netip.MustParseAddr("fe80::1%eth0")}, nil, "fe80::1"},
		{"mapped ip", "", clientinfo.Info{IP: netip.MustParseAddr("::ffff:10.0.0.5")}, nil, "10.0.0.5"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w, ins := newWriter(t)
			ctx := context.Background()
			if c.reqID != "" {
				ctx = requestid.ContextWithID(ctx, c.reqID)
			}
			ctx = clientinfo.ContextWith(ctx, c.info)
			if err := w.Write(ctx, okEntry()); err != nil {
				t.Fatalf("Write: %v", err)
			}
			d := decode(t, ins.Rows()[0].Payload)
			for _, k := range c.missing {
				if _, ok := d.Data[k]; ok {
					t.Errorf("data has %s", k)
				}
			}
			if got, _ := d.Data["ipAddress"].(string); got != c.ip {
				t.Errorf("ipAddress = %q, want %q", got, c.ip)
			}
		})
	}
}

// U18: nil or empty details leave out details.
func TestU18EmptyDetails(t *testing.T) {
	w, ins := newWriter(t)
	for _, d := range []map[string]any{nil, {}} {
		e := okEntry()
		e.Details = d
		if err := w.Write(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range ins.Rows() {
		if _, ok := decode(t, r.Payload).Data["details"]; ok {
			t.Errorf("data has details: %s", r.Payload)
		}
	}
}

func TestWriteSeveralEntriesAtomic(t *testing.T) {
	w, ins := newWriter(t)
	bad := okEntry()
	bad.Result = "x"
	if err := w.Write(context.Background(), okEntry(), bad); !errors.Is(err, audit.ErrInvalidResult) {
		t.Fatalf("err = %v", err)
	}
	if ins.InsertCalls() != 0 {
		t.Error("Insert was called")
	}
	if err := w.Write(context.Background(), okEntry(), okEntry()); err != nil {
		t.Fatal(err)
	}
	if n := len(ins.Rows()); n != 2 {
		t.Errorf("rows = %d, want 2", n)
	}
}
