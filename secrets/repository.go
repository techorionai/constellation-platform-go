package secrets

import (
	"context"
	"errors"
	"time"
)

// Errors of the [Repository] and the [Service]. None contains a value or a key.
var (
	// ErrNotFound means that no secret has the name.
	ErrNotFound = errors.New("secrets: not found")
	// ErrConflict means that the stored envelope is not the expected one.
	ErrConflict = errors.New("secrets: the stored envelope changed")
	// ErrInvalidName means that the name does not match the name rule.
	ErrInvalidName = errors.New("secrets: invalid name")
	// ErrEmptyValue means that the value has no bytes.
	ErrEmptyValue = errors.New("secrets: the value is empty")
	// ErrTooLarge means that the value has more than [MaxValueSize] bytes.
	ErrTooLarge = errors.New("secrets: the value is too large")
	// ErrInvalidLimit means that a list limit is negative or more than
	// [MaxListLimit].
	ErrInvalidLimit = errors.New("secrets: invalid limit")
	// ErrInvalidBatchSize means that the batch size of Rewrap is not from 1 to
	// [MaxListLimit].
	ErrInvalidBatchSize = errors.New("secrets: invalid batch size")
)

// Record is a stored secret.
type Record struct {
	Name      string
	Envelope  Envelope
	CreatedAt time.Time
	UpdatedAt time.Time
}

// UpsertParams are the parameters of [Repository.Upsert].
type UpsertParams struct {
	Name     string
	Envelope Envelope
}

// ListParams are the parameters of a list by name. The cursor is the name.
type ListParams struct {
	// After is the last name of the previous page. Empty gives the first page.
	After string
	// Limit is the page size. For [Service.List], 0 gives 100.
	Limit int
}

// ListResult is a page of names.
type ListResult struct {
	// Names are in ascending order.
	Names []string
	// Next is the last name of the page, or empty when there are no more names.
	Next string
}

// RewrapListParams are the parameters of [Repository.ListForRewrap].
type RewrapListParams struct {
	// NotKeyID is the key ID to skip. It is the ID of the current master key.
	NotKeyID string
	// After is the last name of the previous page.
	After string
	// Limit is the page size.
	Limit int
}

// UpdateEnvelopeParams are the parameters of [Repository.UpdateEnvelope].
type UpdateEnvelopeParams struct {
	Name string
	// Old is the envelope that the caller read. The update changes the row only
	// if the stored envelope is still Old.
	Old Envelope
	New Envelope
}

// Repository stores the envelopes. An adapter joins the transaction of the
// caller if the context has one.
type Repository interface {
	// Upsert makes a row for a new name. For an existing name it replaces the
	// envelope and sets the update time.
	Upsert(ctx context.Context, params UpsertParams) error
	// Get returns the record, or [ErrNotFound].
	Get(ctx context.Context, name string) (Record, error)
	// Delete removes the record, or returns [ErrNotFound].
	Delete(ctx context.Context, name string) error
	// ListNames returns the names in ascending order, after params.After.
	ListNames(ctx context.Context, params ListParams) (ListResult, error)
	// ListForRewrap returns the records that do not use params.NotKeyID, with a
	// name after params.After, in ascending order of name.
	ListForRewrap(ctx context.Context, params RewrapListParams) ([]Record, error)
	// UpdateEnvelope replaces the envelope only if the stored envelope is still
	// params.Old. Else it returns [ErrConflict].
	UpdateEnvelope(ctx context.Context, params UpdateEnvelopeParams) error
}
