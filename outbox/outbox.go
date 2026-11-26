package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/auth"
)

// Errors of the writer. Each error is wrapped with %w, so use errors.Is.
var (
	// ErrNoTransaction means that the context has no database transaction.
	ErrNoTransaction = errors.New("outbox: the context has no transaction")
	// ErrUnknownEventType means that the type does not match the EventType
	// pattern or is not in Config.Types.
	ErrUnknownEventType = errors.New("outbox: unknown event type")
	// ErrInvalidActor means that an actor field breaks a limit of the contract.
	ErrInvalidActor = errors.New("outbox: invalid actor")
	// ErrInvalidSubject means that the subject has more than 256 characters.
	ErrInvalidSubject = errors.New("outbox: invalid subject")
	// ErrInvalidData means that the data does not marshal to a JSON object.
	ErrInvalidData = errors.New("outbox: the data must be a JSON object")
	// ErrEventTooLarge means that the CloudEvent in JSON has more than 256 KiB.
	ErrEventTooLarge = errors.New("outbox: the event is larger than 256 KiB")
	// ErrInvalidConfig means that a Config or an option is not valid.
	ErrInvalidConfig = errors.New("outbox: invalid configuration")
)

// Limits of the contract (lib/events.tsp).
const (
	maxEventBytes   = 256 << 10
	maxActorLen     = 128
	maxSubjectLen   = 256
	maxTypeLen      = 128
	specVersion     = "1.0"
	dataContentType = "application/json"
	// timeLayout has a fixed length, so the size of an event is predictable.
	timeLayout = "2006-01-02T15:04:05.000Z"
)

var (
	appPattern       = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){2,}$`)
)

// Actor is the principal that did the action of an event. It sets the CloudEvent
// attributes actor, actortype, act, and acttype.
type Actor struct {
	// ID is the sub of the principal. It has 1 to 128 characters.
	ID string
	// Type is the type of the principal.
	Type auth.PrincipalType
	// Act is the agent that acts for the principal. Empty means not set.
	Act string
	// ActType is the type of Act. Set it with Act, or set neither.
	ActType auth.PrincipalType
}

// ActorFromPrincipal maps the sub, the principal type, and the act of a verified
// principal. A request handler uses it. Never build an Actor from request data.
func ActorFromPrincipal(p auth.Principal) Actor {
	a := Actor{ID: p.ID, Type: p.Type}
	if p.Act != nil {
		a.Act = p.Act.ID
		a.ActType = p.Act.Type
	}
	return a
}

// ServiceActor returns the actor for an event with no request principal, for
// example an event of a timer or of a consumer. clientID is the client ID of the
// own service client. A consumer does not copy the actor from the event that it
// reacts to.
func ServiceActor(clientID string) Actor {
	return Actor{ID: clientID, Type: auth.PrincipalService}
}

// Event is an event that a service writes.
type Event struct {
	// Type is the event type, for example "helpdesk.ticket.created". It must be
	// in Config.Types.
	Type string
	// Subject is the ID of the resource. Empty means not set. It has a maximum
	// of 256 characters.
	Subject string
	// Data is the data of the event. It must marshal to a JSON object.
	Data any
	// Actor is the principal that did the action.
	Actor Actor
}

// Writer writes events in the transaction of the context.
type Writer interface {
	// Write adds the events to the outbox. The context must have a transaction.
	// The events are all written or none is written.
	Write(ctx context.Context, events ...Event) error
}

// Row is one row of the outbox, before the insert.
type Row struct {
	// EventID is the id of the CloudEvent (a UUIDv7).
	EventID string
	// Type is the event type.
	Type string
	// Payload is the CloudEvent in JSON.
	Payload []byte
}

// Inserter is the port to the storage of the outbox.
type Inserter interface {
	// InTransaction reports whether ctx has a database transaction.
	InTransaction(ctx context.Context) bool
	// Insert adds all rows in the transaction of ctx.
	Insert(ctx context.Context, rows []Row) error
}

// Config is the configuration of the writer.
type Config struct {
	// App is the app key of the service. It must match ^[a-z][a-z0-9_]{0,31}$.
	App string
	// Types are the event types that the service makes. It needs 1 or more.
	Types []string
}

// EventWriter is the [Writer] with the rules of the contract.
type EventWriter struct {
	app      string
	types    map[string]struct{}
	inserter Inserter
}

var _ Writer = (*EventWriter)(nil)

// New checks cfg and makes a writer. It returns an error that wraps
// [ErrInvalidConfig] and names the field.
func New(cfg Config, ins Inserter) (*EventWriter, error) {
	if !appPattern.MatchString(cfg.App) {
		return nil, fmt.Errorf("%w: App must match %s", ErrInvalidConfig, appPattern)
	}
	if len(cfg.Types) == 0 {
		return nil, fmt.Errorf("%w: Types needs 1 or more event types", ErrInvalidConfig)
	}
	if ins == nil {
		return nil, fmt.Errorf("%w: the inserter is required", ErrInvalidConfig)
	}
	types := make(map[string]struct{}, len(cfg.Types))
	for _, t := range cfg.Types {
		if !validType(t) {
			return nil, fmt.Errorf("%w: Types has the invalid event type %q", ErrInvalidConfig, t)
		}
		types[t] = struct{}{}
	}
	return &EventWriter{app: cfg.App, types: types, inserter: ins}, nil
}

func validType(t string) bool {
	return len(t) <= maxTypeLen && eventTypePattern.MatchString(t)
}

// cloudEvent is the JSON form of the event. The field order is fixed.
type cloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Time            string          `json:"time"`
	Subject         string          `json:"subject,omitempty"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
	Actor           string          `json:"actor"`
	ActorType       string          `json:"actortype"`
	Act             string          `json:"act,omitempty"`
	ActType         string          `json:"acttype,omitempty"`
	Traceparent     string          `json:"traceparent,omitempty"`
}

// Write checks the events and inserts them. It returns an error that wraps a
// sentinel error of this package, and no row is written, when one event breaks
// a rule.
func (w *EventWriter) Write(ctx context.Context, events ...Event) error {
	if !w.inserter.InTransaction(ctx) {
		return ErrNoTransaction
	}
	rows := make([]Row, 0, len(events))
	traceparent := traceparentOf(ctx)
	for i := range events {
		row, err := w.build(&events[i], traceparent)
		if err != nil {
			return fmt.Errorf("event %d: %w", i, err)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	return w.inserter.Insert(ctx, rows)
}

func (w *EventWriter) build(e *Event, traceparent string) (Row, error) {
	if _, ok := w.types[e.Type]; !ok {
		return Row{}, fmt.Errorf("%w: %q", ErrUnknownEventType, e.Type)
	}
	if err := checkActor(e.Actor); err != nil {
		return Row{}, err
	}
	if utf8.RuneCountInString(e.Subject) > maxSubjectLen {
		return Row{}, fmt.Errorf("%w: the subject has more than %d characters", ErrInvalidSubject, maxSubjectLen)
	}
	data, err := json.Marshal(e.Data)
	if err != nil {
		return Row{}, fmt.Errorf("%w: %w", ErrInvalidData, err)
	}
	if len(data) == 0 || data[0] != '{' {
		return Row{}, ErrInvalidData
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Row{}, fmt.Errorf("outbox: make event id: %w", err)
	}
	ce := cloudEvent{
		SpecVersion:     specVersion,
		ID:              id.String(),
		Source:          "/" + w.app,
		Type:            e.Type,
		Time:            time.Now().UTC().Format(timeLayout),
		Subject:         e.Subject,
		DataContentType: dataContentType,
		Data:            data,
		Actor:           e.Actor.ID,
		ActorType:       string(e.Actor.Type),
		Act:             e.Actor.Act,
		ActType:         string(e.Actor.ActType),
		Traceparent:     traceparent,
	}
	payload, err := json.Marshal(ce)
	if err != nil {
		return Row{}, fmt.Errorf("%w: %w", ErrInvalidData, err)
	}
	if hasEscapedNUL(payload) {
		return Row{}, fmt.Errorf("%w: a value has the character U+0000", ErrInvalidData)
	}
	if len(payload) > maxEventBytes {
		return Row{}, fmt.Errorf("%w: %d bytes", ErrEventTooLarge, len(payload))
	}
	return Row{EventID: ce.ID, Type: e.Type, Payload: payload}, nil
}

// hasEscapedNUL reports whether the JSON text has the escape \u0000 in a
// string. The jsonb column of PostgreSQL rejects this escape. An escaped
// backslash that is followed by the text u0000 does not count.
func hasEscapedNUL(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' {
			continue
		}
		if i+5 < len(b) && b[i+1] == 'u' && string(b[i+2:i+6]) == "0000" {
			return true
		}
		i++
	}
	return false
}

func checkActor(a Actor) error {
	n := utf8.RuneCountInString(a.ID)
	switch {
	case n < 1 || n > maxActorLen:
		return fmt.Errorf("%w: the actor ID needs 1 to %d characters", ErrInvalidActor, maxActorLen)
	case !a.Type.Valid():
		return fmt.Errorf("%w: unknown actor type %q", ErrInvalidActor, a.Type)
	case (a.Act == "") != (a.ActType == ""):
		return fmt.Errorf("%w: set Act and ActType together", ErrInvalidActor)
	}
	if a.Act != "" {
		if utf8.RuneCountInString(a.Act) > maxActorLen {
			return fmt.Errorf("%w: Act needs 1 to %d characters", ErrInvalidActor, maxActorLen)
		}
		if !a.ActType.Valid() {
			return fmt.Errorf("%w: unknown act type %q", ErrInvalidActor, a.ActType)
		}
	}
	return nil
}

// traceparentOf returns the W3C traceparent of the span in ctx, or "".
func traceparentOf(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return fmt.Sprintf("00-%s-%s-%02x", sc.TraceID(), sc.SpanID(), byte(sc.TraceFlags()))
}
