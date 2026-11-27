package valkey

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"

	"github.com/techorionai/constellation-platform-go/retention"
	"github.com/techorionai/constellation-platform-go/streams"
)

// ErrNoGroup means that the consumer group does not exist.
var ErrNoGroup = streams.ErrNoGroup

// eventField is the field of a stream entry that holds the CloudEvent.
const eventField = "event"

// ACLSelectors returns the two ACL selectors that a service user needs for the
// streams, besides its own pattern ~<app>:*. The first selector lets the
// service write and trim its own stream. The second selector lets it consume
// all streams. The ACL cannot isolate the consumer groups of different
// services. See docs/usage.md.
//
// ACLSelectors panics if app does not match the app name pattern of
// [streams.GroupName], because an app with a space or ")" would add ACL rules.
func ACLSelectors(app string) string {
	mustApp(app)
	return "(~" + streams.StreamKey(app) + " +xadd +xtrim) " +
		"(~" + streams.KeyPrefix + "* +xreadgroup +xack +xautoclaim +xpending +xinfo|groups +xinfo|stream " +
		"+xinfo|consumers +xgroup|create +xgroup|createconsumer +xgroup|delconsumer +xrange)"
}

// AuditProducerSelectors returns the two ACL selectors that a service user
// needs to write its own audit stream: XADD and XTRIM on audit.stream:<app>, and
// SADD on the set audit.streams. Give them to each service user, besides
// [ACLSelectors]. They give no read access.
//
// AuditProducerSelectors panics if app does not match the app name pattern of
// [streams.GroupName].
func AuditProducerSelectors(app string) string {
	mustApp(app)
	return "(~" + streams.KeyFor(streams.Audit, app) + " +xadd +xtrim) " +
		"(~" + streams.AuditIndexKey + " +sadd)"
}

// AuditReaderSelectors returns the two ACL selectors of the audit service user:
// it consumes all audit streams, reads the set audit.streams with SSCAN, and
// removes members from it with SREM. [AuditApps] removes the members that fail
// the app name pattern or have no stream. Give the selectors only to the audit
// service user.
func AuditReaderSelectors() string {
	return "(~" + streams.AuditKeyPrefix + "* +xreadgroup +xack +xautoclaim +xpending +xinfo|groups +xinfo|stream " +
		"+xinfo|consumers +xgroup|create +xgroup|createconsumer +xgroup|delconsumer +xrange) " +
		"(~" + streams.AuditIndexKey + " +sscan +srem)"
}

// mustApp panics if app is not a valid app name. The value comes from the
// configuration of the service and goes into ACL text and key names.
func mustApp(app string) {
	if err := streams.ValidApp(app); err != nil {
		panic(err)
	}
}

func isNoKey(err error) bool {
	var ve *valkeygo.ValkeyError
	return errors.As(err, &ve) && strings.Contains(ve.Error(), "no such key")
}

func isErrPrefix(err error, prefix string) bool {
	var ve *valkeygo.ValkeyError
	return errors.As(err, &ve) && strings.HasPrefix(ve.Error(), prefix)
}

// Option changes a [Publisher] or a [TrimPurger].
type Option func(*options)

type options struct {
	family       streams.Family
	indexRefresh time.Duration
	logger       *slog.Logger
}

// DefaultAuditIndexRefresh is the time after which an audit publisher adds its
// app to the audit index again.
const DefaultAuditIndexRefresh = time.Minute

// WithFamily sets the family of the stream. The default is [streams.Events]. It
// panics if f is not a known family.
func WithFamily(f streams.Family) Option {
	if !f.Valid() {
		panic(fmt.Errorf("streams: unknown family %d", int(f)))
	}
	return func(o *options) { o.family = f }
}

// WithAuditIndexRefresh sets the time after which an audit publisher adds its
// app to the audit index again. The default is [DefaultAuditIndexRefresh]. It
// is for tests.
func WithAuditIndexRefresh(d time.Duration) Option {
	return func(o *options) { o.indexRefresh = d }
}

// WithLogger sets the logger of the publisher. The default is slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(o *options) { o.logger = l }
}

func resolve(opts []Option) options {
	o := options{family: streams.Events, indexRefresh: DefaultAuditIndexRefresh}
	for _, opt := range opts {
		opt(&o)
	}
	if o.logger == nil {
		o.logger = slog.Default()
	}
	return o
}

// Publisher adds events to the stream of one app. It implements
// outbox.Publisher.
type Publisher struct {
	c      valkeygo.Client
	key    string
	app    string
	family streams.Family

	refresh time.Duration
	logger  *slog.Logger
	mu      sync.Mutex
	lastAdd time.Time
}

// NewPublisher returns a publisher for events.stream:<app>, or for
// audit.stream:<app> with [WithFamily]. It sets no MAXLEN, because the retention
// job trims the stream. The service user needs the ACL selectors of
// [ACLSelectors] or [AuditProducerSelectors]. It panics if app is not a valid
// app name.
//
// An audit publisher adds app to the set audit.streams after its first
// successful publish, and again after a successful publish when the last add is
// older than one minute. A failed add is logged at warn level with no data. The
// publish does not fail, and the publisher tries again after the next publish.
func NewPublisher(c valkeygo.Client, app string, opts ...Option) *Publisher {
	mustApp(app)
	o := resolve(opts)
	return &Publisher{
		c: c, key: streams.KeyFor(o.family, app), app: app, family: o.family,
		refresh: o.indexRefresh, logger: o.logger,
	}
}

// Publish sends one XADD for each payload in one pipeline.
func (p *Publisher) Publish(ctx context.Context, payloads [][]byte) error {
	if len(payloads) == 0 {
		return nil
	}
	cmds := make(valkeygo.Commands, 0, len(payloads))
	for _, pl := range payloads {
		cmds = append(cmds, p.c.B().Xadd().Key(p.key).Id("*").FieldValue().FieldValue(eventField, string(pl)).Build())
	}
	for _, res := range p.c.DoMulti(ctx, cmds...) {
		if err := res.Error(); err != nil {
			return fmt.Errorf("streams: XADD %s: %w", p.key, err)
		}
	}
	if p.family == streams.Audit {
		p.addToIndex(ctx)
	}
	return nil
}

// addToIndex adds the app to the audit index when the last add is missing or old.
// It holds the lock only to read and set the time. It sends SADD with no lock, so
// a slow Valkey does not block other callers. A caller that finds a fresh time
// does not send SADD. If SADD fails, the time goes back to its old value.
func (p *Publisher) addToIndex(ctx context.Context) {
	p.mu.Lock()
	prev := p.lastAdd
	if !prev.IsZero() && time.Since(prev) < p.refresh {
		p.mu.Unlock()
		return
	}
	now := time.Now()
	p.lastAdd = now
	p.mu.Unlock()

	err := p.c.Do(ctx, p.c.B().Sadd().Key(streams.AuditIndexKey).Member(p.app).Build()).Error()
	if err != nil {
		p.mu.Lock()
		if p.lastAdd.Equal(now) {
			p.lastAdd = prev
		}
		p.mu.Unlock()
		p.logger.WarnContext(ctx, "streams: cannot add the app to the audit index",
			slog.String("error.type", fmt.Sprintf("%T", err)))
	}
}

// GroupConfig is the configuration of a [Group].
type GroupConfig struct {
	// App is the app of the consumer. The group must be <App> or <App>.<name>.
	App string
	// Stream is the app of the stream to read.
	Stream string
	// Group is the name of the consumer group.
	Group string
	// Consumer is the name of this consumer. The default is <host>-<random>.
	Consumer string
	// Family is the family of the stream. The default is [streams.Events]. The
	// audit service sets [streams.Audit].
	Family streams.Family
	// StartID is the first ID of a new group: "$" (new events only, the default)
	// or "0" (all events in the stream).
	StartID string
}

// Group is one consumer of a consumer group. It implements streams.Group. A
// Group is for one goroutine.
type Group struct {
	c        valkeygo.Client
	app      string
	stream   string
	key      string
	group    string
	consumer string
	startID  string
	cursor   string
}

var _ streams.Group = (*Group)(nil)

// NewGroup checks the configuration and makes a group. It returns an error that
// wraps [streams.ErrInvalidGroup] for a group that the app cannot use.
func NewGroup(c valkeygo.Client, cfg GroupConfig) (*Group, error) {
	if _, err := streams.GroupName(cfg.App, ""); err != nil {
		return nil, err
	}
	if _, err := streams.GroupName(cfg.Stream, ""); err != nil {
		return nil, fmt.Errorf("streams: invalid stream app %q: %w", cfg.Stream, err)
	}
	if !cfg.Family.Valid() {
		return nil, fmt.Errorf("%w: unknown stream family %d", streams.ErrInvalidConfig, int(cfg.Family))
	}
	if err := streams.ValidGroup(cfg.App, cfg.Group); err != nil {
		return nil, err
	}
	switch cfg.StartID {
	case "":
		cfg.StartID = "$"
	case "$", "0":
	default:
		return nil, fmt.Errorf("streams: StartID must be \"$\" or \"0\", got %q", cfg.StartID)
	}
	if cfg.Consumer == "" {
		host, _ := os.Hostname()
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return nil, fmt.Errorf("streams: random consumer name: %w", err)
		}
		cfg.Consumer = host + "-" + hex.EncodeToString(b)
	}
	return &Group{
		c: c, app: cfg.App, stream: cfg.Stream, key: streams.KeyFor(cfg.Family, cfg.Stream),
		group: cfg.Group, consumer: cfg.Consumer, startID: cfg.StartID, cursor: "0-0",
	}, nil
}

// Stream returns the app of the stream.
func (g *Group) Stream() string { return g.stream }

// Name returns the name of the group.
func (g *Group) Name() string { return g.group }

// Consumer returns the name of this consumer.
func (g *Group) Consumer() string { return g.consumer }

// Ensure creates the group and the stream. A group that exists is not an error.
func (g *Group) Ensure(ctx context.Context) error {
	err := g.c.Do(ctx, g.c.B().XgroupCreate().Key(g.key).Group(g.group).Id(g.startID).Mkstream().Build()).Error()
	if err != nil && !isErrPrefix(err, "BUSYGROUP") {
		return fmt.Errorf("streams: XGROUP CREATE %s %s: %w", g.key, g.group, err)
	}
	return nil
}

// recoverGroup creates the group again after a NOGROUP error.
func (g *Group) recoverGroup(ctx context.Context, err error) error {
	if isErrPrefix(err, "NOGROUP") {
		_ = g.Ensure(ctx)
	}
	return err
}

// Read waits up to block for new messages.
func (g *Group) Read(ctx context.Context, count int, block time.Duration) ([]streams.Message, error) {
	ms := max(block.Milliseconds(), 1)
	res := g.c.Do(ctx, g.c.B().Xreadgroup().Group(g.group, g.consumer).Count(int64(count)).
		Block(ms).Streams().Key(g.key).Id(">").Build())
	if err := res.Error(); err != nil {
		if valkeygo.IsValkeyNil(err) {
			return nil, nil
		}
		return nil, g.recoverGroup(ctx, fmt.Errorf("streams: XREADGROUP %s: %w", g.key, err))
	}
	byKey, err := res.AsXRead()
	if err != nil {
		return nil, fmt.Errorf("streams: parse XREADGROUP: %w", err)
	}
	entries := byKey[g.key]
	msgs := make([]streams.Message, 0, len(entries))
	for _, e := range entries {
		msgs = append(msgs, streams.Message{ID: e.ID, Event: []byte(e.FieldValues[eventField]), Deliveries: 1})
	}
	return msgs, nil
}

// Claim takes pending messages that are idle for more than idle. It keeps the
// cursor of XAUTOCLAIM between calls. Then it reads the delivery count of each
// claimed message with XPENDING. A message that another consumer claimed in the
// meantime is not in the result.
func (g *Group) Claim(ctx context.Context, idle time.Duration, count int) ([]streams.Message, []string, error) {
	res := g.c.Do(ctx, g.c.B().Xautoclaim().Key(g.key).Group(g.group).Consumer(g.consumer).
		MinIdleTime(strconv.FormatInt(max(idle.Milliseconds(), 0), 10)).Start(g.cursor).Count(int64(count)).Build())
	arr, err := res.ToArray()
	if err != nil {
		return nil, nil, g.recoverGroup(ctx, fmt.Errorf("streams: XAUTOCLAIM %s: %w", g.key, err))
	}
	if len(arr) < 2 {
		return nil, nil, fmt.Errorf("streams: XAUTOCLAIM %s: reply has %d elements", g.key, len(arr))
	}
	next, err := arr[0].ToString()
	if err != nil {
		return nil, nil, fmt.Errorf("streams: parse XAUTOCLAIM cursor: %w", err)
	}
	g.cursor = next
	entries, err := arr[1].AsXRange()
	if err != nil {
		return nil, nil, fmt.Errorf("streams: parse XAUTOCLAIM entries: %w", err)
	}
	var deleted []string
	if len(arr) > 2 {
		deleted, err = arr[2].AsStrSlice()
		if err != nil {
			return nil, nil, fmt.Errorf("streams: parse XAUTOCLAIM deleted IDs: %w", err)
		}
	}
	if len(entries) == 0 {
		return nil, deleted, nil
	}
	cmds := make(valkeygo.Commands, 0, len(entries))
	for _, e := range entries {
		cmds = append(cmds, g.c.B().Xpending().Key(g.key).Group(g.group).Start(e.ID).End(e.ID).Count(1).Consumer(g.consumer).Build())
	}
	results := g.c.DoMulti(ctx, cmds...)
	msgs := make([]streams.Message, 0, len(entries))
	for i, e := range entries {
		rows, err := results[i].ToArray()
		if err != nil {
			return nil, deleted, fmt.Errorf("streams: XPENDING %s: %w", g.key, err)
		}
		if len(rows) == 0 {
			continue
		}
		fields, err := rows[0].ToArray()
		if err != nil || len(fields) < 4 {
			return nil, deleted, fmt.Errorf("streams: parse XPENDING %s", g.key)
		}
		deliveries, err := fields[3].AsInt64()
		if err != nil {
			return nil, deleted, fmt.Errorf("streams: parse XPENDING delivery count: %w", err)
		}
		msgs = append(msgs, streams.Message{ID: e.ID, Event: []byte(e.FieldValues[eventField]), Deliveries: deliveries})
	}
	return msgs, deleted, nil
}

// Ack acknowledges the messages.
func (g *Group) Ack(ctx context.Context, ids ...string) error {
	if len(ids) == 0 {
		return nil
	}
	if err := g.c.Do(ctx, g.c.B().Xack().Key(g.key).Group(g.group).Id(ids...).Build()).Error(); err != nil {
		return fmt.Errorf("streams: XACK %s: %w", g.key, err)
	}
	return nil
}

// Cleanup deletes the consumers of the group that have no pending messages and
// an idle time of more than idle. It does not delete the own consumer.
func (g *Group) Cleanup(ctx context.Context, idle time.Duration) (int, error) {
	res := g.c.Do(ctx, g.c.B().Arbitrary("XINFO", "CONSUMERS").Keys(g.key).Args(g.group).Build())
	arr, err := res.ToArray()
	if err != nil {
		return 0, fmt.Errorf("streams: XINFO CONSUMERS %s: %w", g.key, err)
	}
	n := 0
	for _, item := range arr {
		m, err := item.AsMap()
		if err != nil {
			return n, fmt.Errorf("streams: parse XINFO CONSUMERS: %w", err)
		}
		name, err := mapString(m, "name")
		if err != nil || name == g.consumer {
			continue
		}
		pending, err1 := mapInt(m, "pending")
		idleMs, err2 := mapInt(m, "idle")
		if err1 != nil || err2 != nil || pending != 0 || idleMs <= idle.Milliseconds() {
			continue
		}
		if err := g.c.Do(ctx, g.c.B().Arbitrary("XGROUP", "DELCONSUMER").Keys(g.key).Args(g.group, name).Build()).Error(); err != nil {
			return n, fmt.Errorf("streams: XGROUP DELCONSUMER %s: %w", g.key, err)
		}
		n++
	}
	return n, nil
}

// Leave deletes the own consumer if it has no pending messages.
func (g *Group) Leave(ctx context.Context) error {
	res := g.c.Do(ctx, g.c.B().Xpending().Key(g.key).Group(g.group).Start("-").End("+").Count(1).Consumer(g.consumer).Build())
	rows, err := res.ToArray()
	if err != nil {
		if isErrPrefix(err, "NOGROUP") {
			return nil
		}
		return fmt.Errorf("streams: XPENDING %s: %w", g.key, err)
	}
	if len(rows) > 0 {
		return nil
	}
	if err := g.c.Do(ctx, g.c.B().Arbitrary("XGROUP", "DELCONSUMER").Keys(g.key).Args(g.group, g.consumer).Build()).Error(); err != nil {
		return fmt.Errorf("streams: XGROUP DELCONSUMER %s: %w", g.key, err)
	}
	return nil
}

// groupInfos returns the fields of all groups of the stream of app in f from
// XINFO GROUPS.
func groupInfos(ctx context.Context, c valkeygo.Client, f streams.Family, app string) ([]map[string]valkeygo.ValkeyMessage, error) {
	res := c.Do(ctx, c.B().Arbitrary("XINFO", "GROUPS").Keys(streams.KeyFor(f, app)).Build())
	arr, err := res.ToArray()
	if err != nil {
		if isNoKey(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("streams: XINFO GROUPS %s: %w", streams.KeyFor(f, app), err)
	}
	out := make([]map[string]valkeygo.ValkeyMessage, 0, len(arr))
	for _, item := range arr {
		m, err := item.AsMap()
		if err != nil {
			return nil, fmt.Errorf("streams: parse XINFO GROUPS: %w", err)
		}
		out = append(out, m)
	}
	return out, nil
}

// Groups returns the sorted names of the consumer groups of the stream of app.
// The list is empty if the stream does not exist.
func Groups(ctx context.Context, c valkeygo.Client, app string) ([]string, error) {
	return GroupsFor(ctx, c, streams.Events, app)
}

// GroupsFor returns the sorted names of the consumer groups of the stream of app
// in the family f. The list is empty if the stream does not exist.
func GroupsFor(ctx context.Context, c valkeygo.Client, f streams.Family, app string) ([]string, error) {
	infos, err := groupInfos(ctx, c, f, app)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(infos))
	for _, m := range infos {
		name, err := mapString(m, "name")
		if err != nil {
			return nil, fmt.Errorf("streams: parse group name: %w", err)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return names, nil
}

// Lag returns the age of the oldest message that the group did not acknowledge.
// This is the older of the smallest pending ID and the first entry after the
// last delivered ID. It returns 0 if there is none, and [ErrNoGroup] if the
// group does not exist.
func Lag(ctx context.Context, c valkeygo.Client, app, group string) (time.Duration, error) {
	return LagFor(ctx, c, streams.Events, app, group)
}

// LagFor is like [Lag] for the stream of app in the family f.
func LagFor(ctx context.Context, c valkeygo.Client, f streams.Family, app, group string) (time.Duration, error) {
	key := streams.KeyFor(f, app)
	infos, err := groupInfos(ctx, c, f, app)
	if err != nil {
		return 0, err
	}
	lastDelivered := ""
	found := false
	for _, m := range infos {
		if name, _ := mapString(m, "name"); name == group {
			found = true
			lastDelivered, err = mapString(m, "last-delivered-id")
			if err != nil {
				return 0, fmt.Errorf("streams: parse last-delivered-id: %w", err)
			}
		}
	}
	if !found {
		return 0, ErrNoGroup
	}
	var ids []string
	sum, err := c.Do(ctx, c.B().Xpending().Key(key).Group(group).Build()).ToArray()
	if err != nil {
		return 0, fmt.Errorf("streams: XPENDING %s: %w", key, err)
	}
	if len(sum) >= 2 {
		if count, _ := sum[0].AsInt64(); count > 0 {
			minID, err := sum[1].ToString()
			if err != nil {
				return 0, fmt.Errorf("streams: parse XPENDING: %w", err)
			}
			ids = append(ids, minID)
		}
	}
	entries, err := c.Do(ctx, c.B().Xrange().Key(key).Start("("+lastDelivered).End("+").Count(1).Build()).AsXRange()
	if err != nil {
		return 0, fmt.Errorf("streams: XRANGE %s: %w", key, err)
	}
	if len(entries) > 0 {
		ids = append(ids, entries[0].ID)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	oldest := int64(-1)
	for _, id := range ids {
		ms, _, _ := strings.Cut(id, "-")
		v, err := strconv.ParseInt(ms, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("streams: parse stream ID %q: %w", id, err)
		}
		if oldest < 0 || v < oldest {
			oldest = v
		}
	}
	return max(time.Since(time.UnixMilli(oldest)), 0), nil
}

// TrimPurger trims the stream of one app. It implements retention.Purger.
type TrimPurger struct {
	c   valkeygo.Client
	key string
}

var _ retention.Purger = (*TrimPurger)(nil)

// NewTrimPurger returns a purger for events.stream:<app>, or for
// audit.stream:<app> with [WithFamily]. It panics if app is
// not a valid app name.
func NewTrimPurger(c valkeygo.Client, app string, opts ...Option) *TrimPurger {
	mustApp(app)
	return &TrimPurger{c: c, key: streams.KeyFor(resolve(opts).family, app)}
}

// Purge deletes all entries that are older than params.Before with the exact
// form XTRIM MINID. It does not use params.Limit: the exact form deletes full
// stream nodes fast, and the approximate form with LIMIT returns a count that is
// less than the limit almost every time, so the runner would stop after one
// call. A count above the limit makes the runner call Purge once more, and that
// call returns 0. It also deletes entries that a group still has pending. The
// consumer counts them as lost.
func (p *TrimPurger) Purge(ctx context.Context, params retention.PurgeParams) (int, error) {
	n, err := p.c.Do(ctx, p.c.B().Xtrim().Key(p.key).Minid().Threshold(strconv.FormatInt(params.Before.UnixMilli(), 10)+"-0").Build()).AsInt64()
	if err != nil {
		return 0, fmt.Errorf("streams: XTRIM %s: %w", p.key, err)
	}
	return int(n), nil
}

func mapString(m map[string]valkeygo.ValkeyMessage, key string) (string, error) {
	v := m[key]
	return v.ToString()
}

func mapInt(m map[string]valkeygo.ValkeyMessage, key string) (int64, error) {
	v := m[key]
	return v.AsInt64()
}

// ErrTooManyAuditApps means that the audit index has more than [MaxAuditApps]
// apps with a stream, or that [AuditApps] read [MaxAuditMembers] members and the
// set has more. [AuditApps] returns the apps that it found (a maximum of
// [MaxAuditApps]) with this error.
var ErrTooManyAuditApps = errors.New("streams: the audit index has too many apps")

// MaxAuditApps is the maximum number of apps that [AuditApps] returns.
const MaxAuditApps = 1000

// MaxAuditMembers is the maximum number of members that one call of
// [AuditApps] reads from the set audit.streams. Each member that the scan reads
// counts, also a member that [AuditApps] deletes. The limit keeps the work of
// one call small.
const MaxAuditMembers = 1000 * MaxAuditApps

// auditMemberLimit is the limit that the scan uses. Only a test changes it.
var auditMemberLimit = MaxAuditMembers

// auditBeforeSrem and auditSecondCheck are hooks that only a test sets.
var (
	auditBeforeSrem  func()
	auditSecondCheck func()
)

// auditCheckChunk is the number of XINFO STREAM commands in one pipeline.
const auditCheckChunk = 200

// auditScanCount is the COUNT hint of each SSCAN command.
const auditScanCount = 500

// AuditApps returns the sorted apps that write an audit stream. It reads the set
// audit.streams with SSCAN. Only a member that matches the app name pattern and
// whose stream audit.stream:<member> exists (XINFO STREAM) counts as an app. For
// each page, it deletes the members that fail the pattern or have no stream with
// one SREM. Then it checks the deleted members that match the pattern one more
// time, because an app can publish between the check and the delete. A member
// whose stream exists then is in the result. A real app is not lost, because
// the publisher sends XADD before SADD.
//
// A deleted member does not count against [MaxAuditApps]. Each member that the
// scan reads counts against [MaxAuditMembers]. If more than [MaxAuditApps]
// members have a stream, or the scan reaches [MaxAuditMembers] and the set has
// more, it returns the apps that it found (a maximum of [MaxAuditApps]) and
// [ErrTooManyAuditApps]. The caller logs the error and uses the list.
//
// An error of SREM does not stop the call. For example, a user without +srem
// gets NOPERM. The members stay in the set and the next call examines them
// again. The user needs [AuditReaderSelectors].
func AuditApps(ctx context.Context, c valkeygo.Client) ([]string, error) {
	var apps []string
	truncated := false
	read := 0
	var cursor uint64
	for {
		entry, err := c.Do(ctx, c.B().Sscan().Key(streams.AuditIndexKey).Cursor(cursor).Count(auditScanCount).Build()).AsScanEntry()
		if err != nil {
			if valkeygo.IsValkeyNil(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("streams: SSCAN %s: %w", streams.AuditIndexKey, err)
		}
		cursor = entry.Cursor
		page := entry.Elements
		if room := auditMemberLimit - read; len(page) > room {
			page = page[:room]
			truncated = true
		}
		read += len(page)
		found, err := auditPage(ctx, c, page)
		if err != nil {
			return nil, err
		}
		apps = append(apps, found...)
		if read >= auditMemberLimit && cursor != 0 {
			truncated = true
		}
		if truncated || cursor == 0 {
			break
		}
	}
	slices.Sort(apps)
	apps = slices.Compact(apps)
	if len(apps) > MaxAuditApps {
		return apps[:MaxAuditApps], ErrTooManyAuditApps
	}
	if truncated {
		return apps, ErrTooManyAuditApps
	}
	return apps, nil
}

// auditPage checks the members of one SSCAN page, deletes the members that fail
// the pattern or have no stream, and returns the apps that have a stream.
func auditPage(ctx context.Context, c valkeygo.Client, page []string) ([]string, error) {
	var remove, candidates []string
	for _, m := range page {
		if streams.ValidApp(m) == nil {
			candidates = append(candidates, m)
		} else {
			remove = append(remove, m)
		}
	}
	found, missing, err := auditHasStream(ctx, c, candidates)
	if err != nil {
		return nil, err
	}
	remove = append(remove, missing...)
	if len(remove) == 0 {
		return found, nil
	}
	if auditBeforeSrem != nil {
		auditBeforeSrem()
	}
	if err := c.Do(ctx, c.B().Srem().Key(streams.AuditIndexKey).Member(remove...).Build()).Error(); err != nil {
		// The members stay in the set. The next call examines them again.
		return found, nil
	}
	if len(missing) > 0 {
		if auditSecondCheck != nil {
			auditSecondCheck()
		}
		again, _, err := auditHasStream(ctx, c, missing)
		if err != nil {
			return nil, err
		}
		found = append(found, again...)
	}
	return found, nil
}

// auditHasStream sends XINFO STREAM for each member. It returns the members with
// a stream and the members with no stream.
func auditHasStream(ctx context.Context, c valkeygo.Client, members []string) (exists, missing []string, err error) {
	for start := 0; start < len(members); start += auditCheckChunk {
		chunk := members[start:min(start+auditCheckChunk, len(members))]
		cmds := make(valkeygo.Commands, 0, len(chunk))
		for _, m := range chunk {
			cmds = append(cmds, c.B().Arbitrary("XINFO", "STREAM").Keys(streams.KeyFor(streams.Audit, m)).Build())
		}
		for i, res := range c.DoMulti(ctx, cmds...) {
			if err := res.Error(); err != nil {
				if isNoKey(err) {
					missing = append(missing, chunk[i])
					continue
				}
				return nil, nil, fmt.Errorf("streams: XINFO STREAM %s: %w", streams.KeyFor(streams.Audit, chunk[i]), err)
			}
			exists = append(exists, chunk[i])
		}
	}
	return exists, missing, nil
}
