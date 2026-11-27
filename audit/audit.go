package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/auth"
	"github.com/techorionai/constellation-platform-go/clientinfo"
	"github.com/techorionai/constellation-platform-go/outbox"
	"github.com/techorionai/constellation-platform-go/requestid"
)

// Errors of the writer. Each error is wrapped with %w, so use errors.Is.
var (
	// ErrNoTransaction means that the context has no database transaction.
	ErrNoTransaction = errors.New("audit: the context has no transaction")
	// ErrInvalidAction means that the action does not match the permission key
	// pattern or does not start with "<app>:".
	ErrInvalidAction = errors.New("audit: invalid action")
	// ErrInvalidResult means that the result is not one of the 4 values.
	ErrInvalidResult = errors.New("audit: invalid result")
	// ErrInvalidTarget means that the target type or the target ID is not valid.
	ErrInvalidTarget = errors.New("audit: invalid target")
	// ErrInvalidSummary means that the summary has more than 1024 characters.
	ErrInvalidSummary = errors.New("audit: invalid summary")
	// ErrInvalidDetails means that the details do not marshal to JSON of a
	// maximum of 16 KiB.
	ErrInvalidDetails = errors.New("audit: invalid details")
	// ErrInvalidActor means that an actor field breaks a limit of the contract.
	ErrInvalidActor = errors.New("audit: invalid actor")
	// ErrInvalidText means that a value has the character U+0000.
	ErrInvalidText = errors.New("audit: a value has the character U+0000")
	// ErrEntryTooLarge means that the CloudEvent in JSON has more than 64 KiB.
	ErrEntryTooLarge = errors.New("audit: the entry is larger than 64 KiB")
	// ErrInvalidConfig means that a Config is not valid.
	ErrInvalidConfig = errors.New("audit: invalid configuration")
)

// Limits of the contract.
const (
	maxEntryBytes   = 64 << 10
	maxDetailsBytes = 16 << 10
	maxActionLen    = 128
	maxActorLen     = 128
	maxTargetIDLen  = 256
	maxSummaryLen   = 1024
	maxUserAgentLen = 512
	specVersion     = "1.0"
	dataContentType = "application/json"
	eventTypeSuffix = ".audit.recorded"
	// timeLayout has a fixed length, so the size of an entry is predictable.
	timeLayout = "2006-01-02T15:04:05.000Z"
)

var (
	appPattern        = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	actionPattern     = regexp.MustCompile(`^(?:[a-z][a-z0-9_]*:[a-z][a-z0-9_]*:[a-z][a-z0-9_]*|identity:admin)$`)
	targetTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	requestIDPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// Result is the result of an audited action.
type Result string

// The values of [Result].
const (
	// Success means that the action changed the state.
	Success Result = "success"
	// Denied means that the policy refused the action.
	Denied Result = "denied"
	// Invalid means that the request was not valid.
	Invalid Result = "invalid"
	// Failure means that a server error stopped the action.
	Failure Result = "failure"
)

// Valid reports whether r is one of the 4 values.
func (r Result) Valid() bool {
	switch r {
	case Success, Denied, Invalid, Failure:
		return true
	}
	return false
}

// Target is the resource that an action changed or read.
type Target struct {
	// Type is the type of the resource. It matches ^[a-z][a-z0-9_]{0,63}$.
	Type string
	// ID is the ID of the resource. It has 1 to 256 characters.
	ID string
}

// Entry is one audit entry. It has no actor field: the writer takes the actor
// from the verified principal in the context.
type Entry struct {
	// Action is the action key in permission key form. It starts with
	// "<app>:" of the producer.
	Action string
	// Result is the result of the action.
	Result Result
	// Target is the resource of the action. Nil means not set.
	Target *Target
	// Summary is a short text of 1 to 1024 characters. Empty means not set. It
	// must have no secret, no token, and no password.
	Summary string
	// Details are more data of a maximum of 16 KiB in JSON. Nil or empty means
	// not set. They must have no secret, no token, and no password.
	Details map[string]any
	// Time is the time of the action. The zero value means now.
	Time time.Time
}

// Config is the configuration of the writer.
type Config struct {
	// App is the app key of the producer. It matches ^[a-z][a-z0-9_]{0,31}$.
	App string
	// ClientID is the client ID of the producer service. It is the actor of an
	// entry that has no principal in the context. It has 1 to 128 characters.
	ClientID string
}

// Inserter is the port to the storage of the audit outbox.
type Inserter interface {
	// InTransaction reports whether ctx has a database transaction.
	InTransaction(ctx context.Context) bool
	// Insert adds all rows in the transaction of ctx.
	Insert(ctx context.Context, rows []outbox.Row) error
}

// Writer writes audit entries in the transaction of the context.
type Writer interface {
	// Write adds the entries to the audit outbox. The context must have a
	// transaction. The entries are all written or none is written.
	Write(ctx context.Context, entries ...Entry) error
}

// EntryWriter is the [Writer] with the rules of the contract.
type EntryWriter struct {
	cfg      Config
	inserter Inserter
}

var _ Writer = (*EntryWriter)(nil)

// New checks cfg and makes a writer. It returns an error that wraps
// [ErrInvalidConfig] and names the field.
func New(ins Inserter, cfg Config) (*EntryWriter, error) {
	if !appPattern.MatchString(cfg.App) {
		return nil, fmt.Errorf("%w: App must match %s", ErrInvalidConfig, appPattern)
	}
	if n := utf8.RuneCountInString(cfg.ClientID); n < 1 || n > maxActorLen {
		return nil, fmt.Errorf("%w: ClientID needs 1 to %d characters", ErrInvalidConfig, maxActorLen)
	}
	if ins == nil {
		return nil, fmt.Errorf("%w: the inserter is required", ErrInvalidConfig)
	}
	return &EntryWriter{cfg: cfg, inserter: ins}, nil
}

type cloudEvent struct {
	SpecVersion     string          `json:"specversion"`
	ID              string          `json:"id"`
	Source          string          `json:"source"`
	Type            string          `json:"type"`
	Time            string          `json:"time"`
	DataContentType string          `json:"datacontenttype"`
	Data            json.RawMessage `json:"data"`
	Actor           string          `json:"actor"`
	ActorType       string          `json:"actortype"`
	Act             string          `json:"act,omitempty"`
	ActType         string          `json:"acttype,omitempty"`
	Traceparent     string          `json:"traceparent,omitempty"`
}

type targetData struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

type entryData struct {
	Action    string          `json:"action"`
	Result    string          `json:"result"`
	Target    *targetData     `json:"target,omitempty"`
	Summary   string          `json:"summary,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
	RequestID string          `json:"requestId,omitempty"`
	IPAddress string          `json:"ipAddress,omitempty"`
	UserAgent string          `json:"userAgent,omitempty"`
}

// Write checks the entries and inserts them. It returns an error that wraps a
// sentinel error of this package, and no row is written, when one entry breaks
// a rule.
func (w *EntryWriter) Write(ctx context.Context, entries ...Entry) error {
	if !w.inserter.InTransaction(ctx) {
		return ErrNoTransaction
	}
	rows := make([]outbox.Row, 0, len(entries))
	m := metaOf(ctx)
	actor := w.actorOf(ctx)
	traceparent := traceparentOf(ctx)
	for i := range entries {
		row, err := w.build(&entries[i], actor, m, traceparent)
		if err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	return w.inserter.Insert(ctx, rows)
}

func (w *EntryWriter) actorOf(ctx context.Context) outbox.Actor {
	if p, ok := auth.PrincipalFromContext(ctx); ok {
		return outbox.ActorFromPrincipal(p)
	}
	return outbox.ServiceActor(w.cfg.ClientID)
}

// meta is the request metadata that the writer copies from the context. A value
// that does not agree with the contract is left out.
type meta struct {
	requestID, ip, userAgent string
}

func metaOf(ctx context.Context) meta {
	var m meta
	if id := requestid.FromContext(ctx); requestIDPattern.MatchString(id) {
		m.requestID = id
	}
	if info, ok := clientinfo.FromContext(ctx); ok {
		if info.IP.IsValid() {
			m.ip = ipText(info.IP)
		}
		if validUserAgent(info.UserAgent) {
			m.userAgent = info.UserAgent
		}
	}
	return m
}

func ipText(a netip.Addr) string {
	s := a.WithZone("").Unmap().String()
	if len(s) > 45 {
		return ""
	}
	return s
}

func validUserAgent(ua string) bool {
	n := utf8.RuneCountInString(ua)
	if n < 1 || n > maxUserAgentLen || !utf8.ValidString(ua) {
		return false
	}
	for _, r := range ua {
		if r == 0 {
			return false
		}
	}
	return true
}

func (w *EntryWriter) build(e *Entry, actor outbox.Actor, m meta, traceparent string) (outbox.Row, error) {
	if err := w.checkEntry(e); err != nil {
		return outbox.Row{}, err
	}
	if err := checkActor(actor); err != nil {
		return outbox.Row{}, err
	}
	data := entryData{
		Action: e.Action, Result: string(e.Result), Summary: e.Summary,
		RequestID: m.requestID, IPAddress: m.ip, UserAgent: m.userAgent,
	}
	if e.Target != nil {
		data.Target = &targetData{Type: e.Target.Type, ID: e.Target.ID}
	}
	if len(e.Details) > 0 {
		b, err := json.Marshal(e.Details)
		if err != nil {
			return outbox.Row{}, fmt.Errorf("%w: %w", ErrInvalidDetails, err)
		}
		if len(b) > maxDetailsBytes {
			return outbox.Row{}, fmt.Errorf("%w: more than %d bytes in JSON", ErrInvalidDetails, maxDetailsBytes)
		}
		data.Details = b
	}
	rawData, err := json.Marshal(data)
	if err != nil {
		return outbox.Row{}, fmt.Errorf("%w: %w", ErrInvalidDetails, err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return outbox.Row{}, fmt.Errorf("audit: make entry id: %w", err)
	}
	t := e.Time
	if t.IsZero() {
		t = time.Now()
	}
	ce := cloudEvent{
		SpecVersion:     specVersion,
		ID:              id.String(),
		Source:          "/" + w.cfg.App,
		Type:            w.cfg.App + eventTypeSuffix,
		Time:            t.UTC().Format(timeLayout),
		DataContentType: dataContentType,
		Data:            rawData,
		Actor:           actor.ID,
		ActorType:       string(actor.Type),
		Act:             actor.Act,
		ActType:         string(actor.ActType),
		Traceparent:     traceparent,
	}
	payload, err := json.Marshal(ce)
	if err != nil {
		return outbox.Row{}, fmt.Errorf("%w: %w", ErrInvalidDetails, err)
	}
	if err := checkPayload(payload); err != nil {
		return outbox.Row{}, err
	}
	return outbox.Row{EventID: ce.ID, Type: ce.Type, Payload: payload}, nil
}

// checkPayload checks the CloudEvent in JSON: no escaped U+0000 and a maximum
// of 64 KiB.
func checkPayload(payload []byte) error {
	if hasEscapedNUL(payload) {
		return ErrInvalidText
	}
	if len(payload) > maxEntryBytes {
		return fmt.Errorf("%w: %d bytes", ErrEntryTooLarge, len(payload))
	}
	return nil
}

func (w *EntryWriter) checkEntry(e *Entry) error {
	prefix := w.cfg.App + ":"
	if len(e.Action) <= len(prefix) || len(e.Action) > maxActionLen || e.Action[:len(prefix)] != prefix ||
		!actionPattern.MatchString(e.Action) {
		return fmt.Errorf("%w: the action must be a permission key that starts with %q", ErrInvalidAction, prefix)
	}
	if !e.Result.Valid() {
		return fmt.Errorf("%w: %q", ErrInvalidResult, e.Result)
	}
	if e.Target != nil {
		if !targetTypePattern.MatchString(e.Target.Type) {
			return fmt.Errorf("%w: the type must match %s", ErrInvalidTarget, targetTypePattern)
		}
		if n := utf8.RuneCountInString(e.Target.ID); n < 1 || n > maxTargetIDLen {
			return fmt.Errorf("%w: the ID needs 1 to %d characters", ErrInvalidTarget, maxTargetIDLen)
		}
	}
	if utf8.RuneCountInString(e.Summary) > maxSummaryLen {
		return fmt.Errorf("%w: more than %d characters", ErrInvalidSummary, maxSummaryLen)
	}
	return nil
}

// hasEscapedNUL reports whether the JSON text has the escape for U+0000 in a
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

func checkActor(a outbox.Actor) error {
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
