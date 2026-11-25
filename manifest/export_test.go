package manifest

import (
	"context"
	"time"
)

// SetSleep replaces the sleep function of the publisher. A test uses it to not
// wait in real time.
func SetSleep(p *Publisher, f func(context.Context, time.Duration) error) { p.sleep = f }
