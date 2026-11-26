package streams

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Errors of this package.
var (
	// ErrInvalidGroup means that a group name is not valid for the app.
	ErrInvalidGroup = errors.New("streams: invalid group")
	// ErrInvalidConfig means that a consumer option is not valid.
	ErrInvalidConfig = errors.New("streams: invalid configuration")
	// ErrNoGroup means that the consumer group does not exist.
	ErrNoGroup = errors.New("streams: the consumer group does not exist")
)

// KeyPrefix is the prefix of each stream key. A service name has no ".", so no
// root pattern ~<svc>:* of a service matches a stream key.
const KeyPrefix = "events.stream:"

var (
	appPattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	namePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
)

// StreamKey returns the key of the stream of an app.
func StreamKey(app string) string { return KeyPrefix + app }

// ValidApp checks that app matches ^[a-z][a-z0-9_]{0,31}$.
func ValidApp(app string) error {
	if !appPattern.MatchString(app) {
		return fmt.Errorf("%w: the app must match %s", ErrInvalidGroup, appPattern)
	}
	return nil
}

// GroupName returns the name of a consumer group of an app: app for an empty
// name, or app.name. app must match ^[a-z][a-z0-9_]{0,31}$. name must match
// ^[a-z][a-z0-9_]{0,31}$.
func GroupName(app, name string) (string, error) {
	if err := ValidApp(app); err != nil {
		return "", err
	}
	if name == "" {
		return app, nil
	}
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("%w: the name must match %s", ErrInvalidGroup, namePattern)
	}
	return app + "." + name, nil
}

// ValidGroup checks that group is <app> or <app>.<name>.
func ValidGroup(app, group string) error {
	if group == app {
		_, err := GroupName(app, "")
		return err
	}
	name, ok := strings.CutPrefix(group, app+".")
	if !ok || name == "" {
		return fmt.Errorf("%w: %q does not belong to the app %q", ErrInvalidGroup, group, app)
	}
	_, err := GroupName(app, name)
	return err
}

// Message is one message of a stream.
type Message struct {
	// ID is the stream ID.
	ID string
	// Event is the CloudEvent in JSON.
	Event []byte
	// Deliveries is the number of deliveries, including this one.
	Deliveries int64
}

// Handler handles one message. It must be idempotent by the source and the id
// of the event, because delivery is at least once. A handler that writes a new
// event uses outbox.ServiceActor. A nil result acknowledges the message. An
// error leaves it pending, and a later claim delivers it again.
type Handler func(ctx context.Context, m Message) error

// Group is the port to one consumer of a consumer group.
type Group interface {
	// Stream returns the app of the stream.
	Stream() string
	// Name returns the name of the group.
	Name() string
	// Ensure creates the group and the stream if they do not exist.
	Ensure(ctx context.Context) error
	// Read waits up to block for new messages. Each message has Deliveries 1.
	Read(ctx context.Context, count int, block time.Duration) ([]Message, error)
	// Claim takes pending messages that are idle for more than idle. deleted are
	// the IDs of pending entries that the stream does not have any more.
	Claim(ctx context.Context, idle time.Duration, count int) (msgs []Message, deleted []string, err error)
	// Ack acknowledges the messages.
	Ack(ctx context.Context, ids ...string) error
	// Cleanup deletes the consumers of the group that have no pending messages
	// and an idle time of more than idle. It returns the count.
	Cleanup(ctx context.Context, idle time.Duration) (int, error)
	// Leave deletes the own consumer if it has no pending messages.
	Leave(ctx context.Context) error
}
