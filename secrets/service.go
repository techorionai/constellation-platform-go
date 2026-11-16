package secrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	// MaxValueSize is the maximum size of a secret value, in bytes.
	MaxValueSize = 64 * 1024
	// DefaultListLimit is the page size of [Service.List] when the limit is 0.
	DefaultListLimit = 100
	// MaxListLimit is the maximum page size and the maximum batch size.
	MaxListLimit = 1000

	scopeName = "github.com/techorionai/constellation-platform-go/secrets"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,199}$`)

// Service stores, reads, and rewraps secrets.
type Service struct {
	ring *KeyRing
	repo Repository
}

// NewService makes a service.
func NewService(ring *KeyRing, repo Repository) *Service {
	return &Service{ring: ring, repo: repo}
}

// RewrapResult is the result of [Service.Rewrap].
type RewrapResult struct {
	// Rewrapped is the number of records that now use the current master key.
	Rewrapped int
	// Conflicts is the number of records that changed during the rewrap. The
	// operator runs Rewrap again.
	Conflicts int
	// Failed is the number of records that no key of the ring can open. The
	// operator examines each one.
	Failed int
}

// span starts a span with the attribute secret.name. The span never gets a
// value, a key, or an error text.
func span(ctx context.Context, op, name string) (context.Context, trace.Span) {
	var attrs []trace.SpanStartOption
	if name != "" {
		attrs = append(attrs, trace.WithAttributes(attribute.String("secret.name", name)))
	}
	return otel.Tracer(scopeName).Start(ctx, "secrets."+op, attrs...)
}

// finish ends the span. An error sets the status to Error with a fixed text,
// because the text of an error from the repository is not under the control of
// this package.
func finish(s trace.Span, err error, expected ...error) {
	if err != nil {
		isExpected := false
		for _, e := range expected {
			if errors.Is(err, e) {
				isExpected = true
			}
		}
		s.SetAttributes(attribute.String("error.type", fmt.Sprintf("%T", rootErr(err))))
		if !isExpected {
			s.SetStatus(codes.Error, "secrets error")
		}
	}
	s.End()
}

func rootErr(err error) error {
	for {
		next := errors.Unwrap(err)
		if next == nil {
			return err
		}
		err = next
	}
}

func checkName(name string) error {
	if !namePattern.MatchString(name) {
		return ErrInvalidName
	}
	return nil
}

// Put encrypts value and stores it under name. A second Put with the same name
// replaces the value. The name must match ^[a-z0-9][a-z0-9._:/-]{0,199}$. The
// value has 1 byte to 64 KiB.
func (s *Service) Put(ctx context.Context, name string, value []byte) (err error) {
	ctx, sp := span(ctx, "put", name)
	defer func() { finish(sp, err, ErrInvalidName, ErrEmptyValue, ErrTooLarge) }()
	if err := checkName(name); err != nil {
		return err
	}
	if len(value) == 0 {
		return ErrEmptyValue
	}
	if len(value) > MaxValueSize {
		return ErrTooLarge
	}
	env, err := Seal(s.ring, value, DataAAD(name))
	if err != nil {
		return err
	}
	return s.repo.Upsert(ctx, UpsertParams{Name: name, Envelope: env})
}

// Get returns the value of the secret name. It returns [ErrNotFound] if there is
// none, and [ErrDecrypt], [ErrUnknownKey], or [ErrUnsupportedFormat] if the
// stored envelope cannot be opened.
func (s *Service) Get(ctx context.Context, name string) (value []byte, err error) {
	ctx, sp := span(ctx, "get", name)
	defer func() { finish(sp, err, ErrInvalidName, ErrNotFound) }()
	if err := checkName(name); err != nil {
		return nil, err
	}
	rec, err := s.repo.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	return Open(s.ring, rec.Envelope, DataAAD(name))
}

// Delete removes the secret name. It returns [ErrNotFound] if there is none.
func (s *Service) Delete(ctx context.Context, name string) (err error) {
	ctx, sp := span(ctx, "delete", name)
	defer func() { finish(sp, err, ErrInvalidName, ErrNotFound) }()
	if err := checkName(name); err != nil {
		return err
	}
	return s.repo.Delete(ctx, name)
}

// List returns a page of secret names. It never returns a value. A limit of 0
// gives 100. A negative limit or a limit of more than 1000 returns
// [ErrInvalidLimit].
func (s *Service) List(ctx context.Context, params ListParams) (res ListResult, err error) {
	ctx, sp := span(ctx, "list", "")
	defer func() { finish(sp, err, ErrInvalidLimit) }()
	if params.Limit < 0 || params.Limit > MaxListLimit {
		return ListResult{}, ErrInvalidLimit
	}
	if params.Limit == 0 {
		params.Limit = DefaultListLimit
	}
	return s.repo.ListNames(ctx, params)
}

// Rewrap wraps the data key of each record again with the current master key.
// It reads the records in pages of batchSize (1 to 1000) with a cursor by name,
// so that one call reads each record one time and always stops.
//
// A record that changed during the rewrap counts in Conflicts. A record that the
// ring cannot open counts in Failed, and Rewrap writes an error log record with
// its name and the kind of the error. Another error of the repository stops
// Rewrap. It returns the counts so far and the error. Rewrap also stops when ctx
// ends.
func (s *Service) Rewrap(ctx context.Context, batchSize int) (res RewrapResult, err error) {
	ctx, sp := span(ctx, "rewrap", "")
	defer func() { finish(sp, err, ErrInvalidBatchSize) }()
	if batchSize < 1 || batchSize > MaxListLimit {
		return res, ErrInvalidBatchSize
	}
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		recs, err := s.repo.ListForRewrap(ctx, RewrapListParams{
			NotKeyID: s.ring.CurrentKeyID(), After: after, Limit: batchSize,
		})
		if err != nil {
			return res, err
		}
		for _, rec := range recs {
			newEnv, err := Rewrap(s.ring, rec.Envelope)
			if err != nil {
				kind := errorKind(err)
				if kind == "" {
					return res, err
				}
				res.Failed++
				slog.ErrorContext(ctx, "secrets: cannot rewrap a record",
					slog.String("secret.name", rec.Name), slog.String("error.kind", kind))
				continue
			}
			err = s.repo.UpdateEnvelope(ctx, UpdateEnvelopeParams{Name: rec.Name, Old: rec.Envelope, New: newEnv})
			switch {
			case err == nil:
				res.Rewrapped++
			case errors.Is(err, ErrConflict):
				res.Conflicts++
			default:
				return res, err
			}
		}
		if len(recs) < batchSize {
			return res, nil
		}
		after = recs[len(recs)-1].Name
	}
}

// errorKind returns the kind of an error that means "this record cannot be
// opened". It returns "" for another error.
func errorKind(err error) string {
	switch {
	case errors.Is(err, ErrUnknownKey):
		return "unknown_key"
	case errors.Is(err, ErrDecrypt):
		return "decrypt"
	case errors.Is(err, ErrUnsupportedFormat):
		return "unsupported_format"
	default:
		return ""
	}
}
