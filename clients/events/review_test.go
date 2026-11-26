package events_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/techorionai/constellation-platform-go/clients/events"
	"github.com/techorionai/constellation-platform-go/eventtypes"
)

const marker = "SECRET-MARKER-TEXT"

func markedReg() eventtypes.Registration {
	r := testReg
	r.EventTypes = append([]eventtypes.EventType(nil), r.EventTypes...)
	for i := range r.EventTypes {
		r.EventTypes[i].Description = "Description " + marker
		r.EventTypes[i].DataSchema = json.RawMessage(`{"type":"object","description":"` + marker + `"}`)
	}
	return r
}

// Issue 22 U12, span part: no span attribute and no span event has a dataSchema
// or a description. The test runs the adapter and the publisher.
func TestU12NoSchemaInSpans(t *testing.T) {
	responses := []struct {
		name   string
		status int
		body   string
	}{
		{"ok", 200, `{"app":"helpdesk","version":1,"eventTypeCount":2,"registeredAt":"2026-01-01T00:00:00Z"}`},
		{"invalid", 422, `{"code":"invalid_data_schema","errors":[{"code":"x","field":"/eventTypes/0/dataSchema"}]}`},
		{"unavailable", 503, `{}`},
	}
	for _, rc := range responses {
		t.Run(rc.name, func(t *testing.T) {
			var sent string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				sent = string(b)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(rc.status)
				_, _ = w.Write([]byte(rc.body))
			}))
			t.Cleanup(srv.Close)
			exp := tracetest.NewInMemoryExporter()
			tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
			reg := events.NewEventTypeRegistry(newClient(t, srv.URL, okTokens(), events.WithTracerProvider(tp)))
			_, _ = reg.Register(context.Background(), markedReg())
			if !strings.Contains(sent, marker) {
				t.Fatalf("the request body has no marker: %s", sent)
			}
			spans := exp.GetSpans()
			if len(spans) == 0 {
				t.Fatal("no span")
			}
			for _, s := range spans {
				for _, a := range s.Attributes {
					if strings.Contains(a.Value.String(), marker) || strings.Contains(string(a.Key), marker) {
						t.Errorf("span %s has the marker in attribute %s", s.Name, a.Key)
					}
				}
				for _, e := range s.Events {
					for _, a := range e.Attributes {
						if strings.Contains(a.Value.String(), marker) {
							t.Errorf("span %s event %s has the marker", s.Name, e.Name)
						}
					}
					if strings.Contains(e.Name, marker) {
						t.Errorf("span %s has an event name with the marker", s.Name)
					}
				}
				if strings.Contains(s.Name, marker) || strings.Contains(s.Status.Description, marker) {
					t.Errorf("span %s has the marker in its name or status", s.Name)
				}
			}
		})
	}
}

// Review R6: the body has the digits of a large integer. A float64 loses them.
func TestRegisterKeepsLargeIntegers(t *testing.T) {
	var sent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		sent = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"app":"helpdesk","version":1,"eventTypeCount":1,"registeredAt":"2026-01-01T00:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)
	r := testReg
	r.EventTypes = []eventtypes.EventType{{Type: "helpdesk.ticket.created", Description: "Created.",
		DataSchema: json.RawMessage(`{"type":"integer","maximum":9007199254740993}`)}}
	if _, err := events.NewEventTypeRegistry(newClient(t, srv.URL, okTokens())).Register(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "9007199254740993") {
		t.Errorf("the body lost the integer: %s", sent)
	}
}

// Review R7: the field of the fallback error has the index of the event type.
func TestRegisterBadSchemaField(t *testing.T) {
	r := testReg
	r.EventTypes = []eventtypes.EventType{
		{Type: "helpdesk.ticket.created", Description: "A.", DataSchema: json.RawMessage(`{"type":"object"}`)},
		{Type: "helpdesk.ticket.closed", Description: "B.", DataSchema: json.RawMessage(`not json`)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("request sent") }))
	t.Cleanup(srv.Close)
	_, err := events.NewEventTypeRegistry(newClient(t, srv.URL, okTokens())).Register(context.Background(), r)
	var ie *eventtypes.InvalidError
	ok := errors.As(err, &ie)
	if !ok || len(ie.Problems) != 1 || ie.Problems[0].Field != "/eventTypes/1/dataSchema" {
		t.Fatalf("err = %#v", err)
	}
}
