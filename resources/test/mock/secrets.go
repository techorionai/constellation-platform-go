package mock

import (
	"context"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/techorionai/constellation-platform-go/secrets"
)

var _ secrets.Repository = (*SecretsRepository)(nil)

// SecretsRepository is a mock of [secrets.Repository]. If a function field is
// nil, the method uses an in-memory map of the records. The map orders names by
// byte value. The mock follows the rules of the PostgreSQL adapter: Get and
// Delete return [secrets.ErrNotFound] for an unknown name, and UpdateEnvelope
// returns [secrets.ErrConflict] when the stored envelope is not Old.
type SecretsRepository struct {
	UpsertFunc         func(ctx context.Context, params secrets.UpsertParams) error
	GetFunc            func(ctx context.Context, name string) (secrets.Record, error)
	DeleteFunc         func(ctx context.Context, name string) error
	ListNamesFunc      func(ctx context.Context, params secrets.ListParams) (secrets.ListResult, error)
	ListForRewrapFunc  func(ctx context.Context, params secrets.RewrapListParams) ([]secrets.Record, error)
	UpdateEnvelopeFunc func(ctx context.Context, params secrets.UpdateEnvelopeParams) error

	mu                sync.Mutex
	records           map[string]secrets.Record
	upsertCalls       int
	getCalls          int
	deleteCalls       int
	listNamesCalls    int
	listParams        []secrets.ListParams
	listForRewrap     int
	rewrapParams      []secrets.RewrapListParams
	updateEnvelopeCnt int
}

// Upsert calls UpsertFunc, or stores the record in the map.
func (r *SecretsRepository) Upsert(ctx context.Context, p secrets.UpsertParams) error {
	r.mu.Lock()
	r.upsertCalls++
	r.mu.Unlock()
	if r.UpsertFunc != nil {
		return r.UpsertFunc(ctx, p)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	rec, ok := r.records[p.Name]
	if !ok {
		rec = secrets.Record{Name: p.Name, CreatedAt: now}
	}
	rec.Envelope = clone(p.Envelope)
	rec.UpdatedAt = now
	r.put(rec)
	return nil
}

// Get calls GetFunc, or reads the map.
func (r *SecretsRepository) Get(ctx context.Context, name string) (secrets.Record, error) {
	r.mu.Lock()
	r.getCalls++
	r.mu.Unlock()
	if r.GetFunc != nil {
		return r.GetFunc(ctx, name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[name]
	if !ok {
		return secrets.Record{}, secrets.ErrNotFound
	}
	rec.Envelope = clone(rec.Envelope)
	return rec, nil
}

// Delete calls DeleteFunc, or removes the record from the map.
func (r *SecretsRepository) Delete(ctx context.Context, name string) error {
	r.mu.Lock()
	r.deleteCalls++
	r.mu.Unlock()
	if r.DeleteFunc != nil {
		return r.DeleteFunc(ctx, name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.records[name]; !ok {
		return secrets.ErrNotFound
	}
	delete(r.records, name)
	return nil
}

// ListNames calls ListNamesFunc, or lists the map.
func (r *SecretsRepository) ListNames(ctx context.Context, p secrets.ListParams) (secrets.ListResult, error) {
	r.mu.Lock()
	r.listNamesCalls++
	r.listParams = append(r.listParams, p)
	r.mu.Unlock()
	if r.ListNamesFunc != nil {
		return r.ListNamesFunc(ctx, p)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for _, name := range r.sortedNames() {
		if name > p.After {
			names = append(names, name)
		}
	}
	res := secrets.ListResult{Names: names}
	if p.Limit > 0 && len(names) > p.Limit {
		res.Names = names[:p.Limit]
		res.Next = names[p.Limit-1]
	}
	return res, nil
}

// ListForRewrap calls ListForRewrapFunc, or lists the map.
func (r *SecretsRepository) ListForRewrap(ctx context.Context, p secrets.RewrapListParams) ([]secrets.Record, error) {
	r.mu.Lock()
	r.listForRewrap++
	r.rewrapParams = append(r.rewrapParams, p)
	r.mu.Unlock()
	if r.ListForRewrapFunc != nil {
		return r.ListForRewrapFunc(ctx, p)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []secrets.Record
	for _, name := range r.sortedNames() {
		rec := r.records[name]
		if name > p.After && rec.Envelope.KeyID != p.NotKeyID {
			rec.Envelope = clone(rec.Envelope)
			out = append(out, rec)
		}
		if p.Limit > 0 && len(out) == p.Limit {
			break
		}
	}
	return out, nil
}

// UpdateEnvelope calls UpdateEnvelopeFunc, or compares and sets in the map.
func (r *SecretsRepository) UpdateEnvelope(ctx context.Context, p secrets.UpdateEnvelopeParams) error {
	r.mu.Lock()
	r.updateEnvelopeCnt++
	r.mu.Unlock()
	if r.UpdateEnvelopeFunc != nil {
		return r.UpdateEnvelopeFunc(ctx, p)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[p.Name]
	if !ok || rec.Envelope.KeyID != p.Old.KeyID || !slices.Equal(rec.Envelope.WrappedDEK, p.Old.WrappedDEK) {
		return secrets.ErrConflict
	}
	rec.Envelope = clone(p.New)
	rec.UpdatedAt = time.Now()
	r.put(rec)
	return nil
}

// Put stores a record in the map without a call count. Use it to set up a test.
func (r *SecretsRepository) Put(name string, env secrets.Envelope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.put(secrets.Record{Name: name, Envelope: clone(env), CreatedAt: now, UpdatedAt: now})
}

// Record returns the record in the map, and false if there is none.
func (r *SecretsRepository) Record(name string) (secrets.Record, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rec, ok := r.records[name]
	rec.Envelope = clone(rec.Envelope)
	return rec, ok
}

// UpsertCalls returns the number of Upsert calls.
func (r *SecretsRepository) UpsertCalls() int { return r.count(&r.upsertCalls) }

// GetCalls returns the number of Get calls.
func (r *SecretsRepository) GetCalls() int { return r.count(&r.getCalls) }

// DeleteCalls returns the number of Delete calls.
func (r *SecretsRepository) DeleteCalls() int { return r.count(&r.deleteCalls) }

// ListNamesCalls returns the number of ListNames calls.
func (r *SecretsRepository) ListNamesCalls() int { return r.count(&r.listNamesCalls) }

// ListForRewrapCalls returns the number of ListForRewrap calls.
func (r *SecretsRepository) ListForRewrapCalls() int { return r.count(&r.listForRewrap) }

// UpdateEnvelopeCalls returns the number of UpdateEnvelope calls.
func (r *SecretsRepository) UpdateEnvelopeCalls() int { return r.count(&r.updateEnvelopeCnt) }

// ListNamesParams returns the parameters of each ListNames call.
func (r *SecretsRepository) ListNamesParams() []secrets.ListParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.listParams)
}

// ListForRewrapParams returns the parameters of each ListForRewrap call.
func (r *SecretsRepository) ListForRewrapParams() []secrets.RewrapListParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.rewrapParams)
}

func (r *SecretsRepository) count(n *int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return *n
}

// put stores rec. The caller holds the lock.
func (r *SecretsRepository) put(rec secrets.Record) {
	if r.records == nil {
		r.records = map[string]secrets.Record{}
	}
	r.records[rec.Name] = rec
}

func (r *SecretsRepository) sortedNames() []string {
	names := make([]string, 0, len(r.records))
	for name := range r.records {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func clone(e secrets.Envelope) secrets.Envelope {
	e.WrappedDEK = slices.Clone(e.WrappedDEK)
	e.Nonce = slices.Clone(e.Nonce)
	e.Ciphertext = slices.Clone(e.Ciphertext)
	return e
}
