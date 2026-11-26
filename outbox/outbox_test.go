package outbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/outbox"
	"github.com/techorionai/constellation-platform-go/resources/test/mock"
)

const eventType = "helpdesk.ticket.created"

func newWriter(t testing.TB, ins *mock.OutboxInserter) *outbox.EventWriter {
	t.Helper()
	w, err := outbox.New(outbox.Config{App: "helpdesk", Types: []string{eventType}}, ins)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func validEvent() outbox.Event {
	return outbox.Event{Type: eventType, Data: map[string]any{"id": "T-1"}, Actor: outbox.ServiceActor("helpdesk-svc")}
}

// U1: a write with no transaction in the context.
func TestU1WriteWithoutTransaction(t *testing.T) {
	ins := &mock.OutboxInserter{NoTx: true}
	err := newWriter(t, ins).Write(context.Background(), validEvent())
	if !errors.Is(err, outbox.ErrNoTransaction) {
		t.Fatalf("err = %v, want ErrNoTransaction", err)
	}
	if len(ins.Rows()) != 0 {
		t.Fatal("a row was written")
	}
}

// U2: an unknown type, and a type that does not match the pattern.
func TestU2UnknownType(t *testing.T) {
	for _, typ := range []string{"helpdesk.ticket.deleted", "Bad Type", "helpdesk.ticket"} {
		ins := &mock.OutboxInserter{}
		e := validEvent()
		e.Type = typ
		if err := newWriter(t, ins).Write(context.Background(), e); !errors.Is(err, outbox.ErrUnknownEventType) {
			t.Errorf("type %q: err = %v, want ErrUnknownEventType", typ, err)
		}
		if len(ins.Rows()) != 0 {
			t.Errorf("type %q: a row was written", typ)
		}
	}
}

// U3: the limits of the actor fields.
func TestU3Actor(t *testing.T) {
	long129 := strings.Repeat("a", 129)
	long128 := strings.Repeat("a", 128)
	bad := map[string]outbox.Actor{
		"no id":           {Type: auth.PrincipalService},
		"id 129":          {ID: long129, Type: auth.PrincipalService},
		"unknown type":    {ID: "x", Type: "robot"},
		"act no acttype":  {ID: "x", Type: auth.PrincipalUser, Act: "agent-1"},
		"acttype no act":  {ID: "x", Type: auth.PrincipalUser, ActType: auth.PrincipalAgent},
		"act 129":         {ID: "x", Type: auth.PrincipalUser, Act: long129, ActType: auth.PrincipalAgent},
		"unknown acttype": {ID: "x", Type: auth.PrincipalUser, Act: "agent-1", ActType: "robot"},
	}
	for name, a := range bad {
		e := validEvent()
		e.Actor = a
		if err := newWriter(t, &mock.OutboxInserter{}).Write(context.Background(), e); !errors.Is(err, outbox.ErrInvalidActor) {
			t.Errorf("%s: err = %v, want ErrInvalidActor", name, err)
		}
	}
	e := validEvent()
	e.Actor = outbox.Actor{ID: long128, Type: auth.PrincipalUser, Act: long128, ActType: auth.PrincipalAgent}
	if err := newWriter(t, &mock.OutboxInserter{}).Write(context.Background(), e); err != nil {
		t.Fatalf("128 characters: %v", err)
	}
}

// U4: the limits of the subject.
func TestU4Subject(t *testing.T) {
	e := validEvent()
	e.Subject = strings.Repeat("s", 257)
	if err := newWriter(t, &mock.OutboxInserter{}).Write(context.Background(), e); !errors.Is(err, outbox.ErrInvalidSubject) {
		t.Fatalf("257: err = %v, want ErrInvalidSubject", err)
	}
	e.Subject = strings.Repeat("s", 256)
	if err := newWriter(t, &mock.OutboxInserter{}).Write(context.Background(), e); err != nil {
		t.Fatalf("256: %v", err)
	}
	ins := &mock.OutboxInserter{}
	e.Subject = ""
	if err := newWriter(t, ins).Write(context.Background(), e); err != nil {
		t.Fatalf("empty: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(ins.Rows()[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["subject"]; ok {
		t.Fatal("an empty subject must not set the attribute")
	}
}

// U5: the limits of the data and of the event size.
func TestU5Data(t *testing.T) {
	ctx := context.Background()
	e := validEvent()
	e.Data = []string{"a"}
	if err := newWriter(t, &mock.OutboxInserter{}).Write(ctx, e); !errors.Is(err, outbox.ErrInvalidData) {
		t.Fatalf("array: err = %v, want ErrInvalidData", err)
	}
	e.Data = nil
	if err := newWriter(t, &mock.OutboxInserter{}).Write(ctx, e); !errors.Is(err, outbox.ErrInvalidData) {
		t.Fatalf("nil: err = %v, want ErrInvalidData", err)
	}
	e.Data = make(chan int)
	if err := newWriter(t, &mock.OutboxInserter{}).Write(ctx, e); !errors.Is(err, outbox.ErrInvalidData) {
		t.Fatalf("chan: err = %v, want ErrInvalidData", err)
	}

	probe := &mock.OutboxInserter{}
	e.Data = map[string]string{"p": ""}
	if err := newWriter(t, probe).Write(ctx, e); err != nil {
		t.Fatal(err)
	}
	pad := 256<<10 - len(probe.Rows()[0].Payload)
	e.Data = map[string]string{"p": strings.Repeat("a", pad)}
	ins := &mock.OutboxInserter{}
	if err := newWriter(t, ins).Write(ctx, e); err != nil {
		t.Fatalf("256 KiB: %v", err)
	}
	if n := len(ins.Rows()[0].Payload); n != 256<<10 {
		t.Fatalf("payload size = %d, want %d", n, 256<<10)
	}
	e.Data = map[string]string{"p": strings.Repeat("a", pad+1)}
	if err := newWriter(t, &mock.OutboxInserter{}).Write(ctx, e); !errors.Is(err, outbox.ErrEventTooLarge) {
		t.Fatalf("256 KiB + 1: err = %v, want ErrEventTooLarge", err)
	}
}

// U6: ActorFromPrincipal.
func TestU6ActorFromPrincipal(t *testing.T) {
	got := outbox.ActorFromPrincipal(auth.Principal{ID: "u1", Type: auth.PrincipalUser})
	if got != (outbox.Actor{ID: "u1", Type: auth.PrincipalUser}) {
		t.Errorf("user: %+v", got)
	}
	got = outbox.ActorFromPrincipal(auth.Principal{ID: "u1", Type: auth.PrincipalUser, Act: &auth.Actor{ID: "a1", Type: auth.PrincipalAgent}})
	if got != (outbox.Actor{ID: "u1", Type: auth.PrincipalUser, Act: "a1", ActType: auth.PrincipalAgent}) {
		t.Errorf("agent: %+v", got)
	}
	got = outbox.ActorFromPrincipal(auth.Principal{ID: "svc", Type: auth.PrincipalService})
	if got != (outbox.Actor{ID: "svc", Type: auth.PrincipalService}) {
		t.Errorf("service: %+v", got)
	}
}

// U7: ServiceActor.
func TestU7ServiceActor(t *testing.T) {
	got := outbox.ServiceActor("helpdesk")
	if got.ID != "helpdesk" || got.Type != auth.PrincipalService || got.Act != "" || got.ActType != "" {
		t.Errorf("ServiceActor = %+v", got)
	}
	e := validEvent()
	e.Actor = outbox.ServiceActor("")
	if err := newWriter(t, &mock.OutboxInserter{}).Write(context.Background(), e); !errors.Is(err, outbox.ErrInvalidActor) {
		t.Fatalf("err = %v, want ErrInvalidActor", err)
	}
}

// U8: the attributes of the CloudEvent.
func TestU8CloudEvent(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	ctx, span := tp.Tracer("test").Start(context.Background(), "request")
	defer span.End()

	ins := &mock.OutboxInserter{}
	e := validEvent()
	e.Subject = "T-1"
	e.Actor = outbox.Actor{ID: "u1", Type: auth.PrincipalUser, Act: "a1", ActType: auth.PrincipalAgent}
	if err := newWriter(t, ins).Write(ctx, e, e); err != nil {
		t.Fatal(err)
	}
	rows := ins.Rows()
	if len(rows) != 2 || rows[0].EventID == rows[1].EventID {
		t.Fatalf("rows = %d, want 2 different IDs", len(rows))
	}
	var got map[string]any
	if err := json.Unmarshal(rows[0].Payload, &got); err != nil {
		t.Fatal(err)
	}
	id, err := uuid.Parse(got["id"].(string))
	if err != nil || id.Version() != 7 || got["id"] != rows[0].EventID {
		t.Errorf("id = %v, %v", got["id"], err)
	}
	sc := span.SpanContext()
	want := map[string]any{
		"specversion": "1.0", "source": "/helpdesk", "type": eventType, "subject": "T-1",
		"datacontenttype": "application/json", "actor": "u1", "actortype": "user", "act": "a1", "acttype": "agent",
		"traceparent": "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-01",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if got["data"].(map[string]any)["id"] != "T-1" {
		t.Errorf("data = %v", got["data"])
	}
	if ts, _ := got["time"].(string); !strings.HasSuffix(ts, "Z") {
		t.Errorf("time = %v, want UTC", got["time"])
	}
	if rows[0].Type != eventType {
		t.Errorf("row type = %q", rows[0].Type)
	}
}

// U9: the configuration.
func TestU9Config(t *testing.T) {
	ins := &mock.OutboxInserter{}
	cases := []struct {
		field string
		cfg   outbox.Config
	}{
		{"App", outbox.Config{Types: []string{eventType}}},
		{"App", outbox.Config{App: "Bad-App", Types: []string{eventType}}},
		{"Types", outbox.Config{App: "helpdesk"}},
		{"Types", outbox.Config{App: "helpdesk", Types: []string{"bad"}}},
	}
	for _, c := range cases {
		_, err := outbox.New(c.cfg, ins)
		if !errors.Is(err, outbox.ErrInvalidConfig) || !strings.Contains(err.Error(), c.field) {
			t.Errorf("%s: err = %v", c.field, err)
		}
	}
}
