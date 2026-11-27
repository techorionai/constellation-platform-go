package valkey

import (
	"context"

	valkeygo "github.com/valkey-io/valkey-go"
)

// AuditHooks sets the limits and the hooks of one call of AuditApps. A zero
// limit or a nil hook keeps the default. Only tests use it.
type AuditHooks struct {
	MemberLimit int
	NoSremLimit int
	BeforeSrem  func()
	SecondCheck func()
}

// AuditAppsWith runs AuditApps with the limits and hooks of h.
func AuditAppsWith(ctx context.Context, c valkeygo.Client, h AuditHooks) ([]string, error) {
	opts := defaultAuditOptions()
	if h.MemberLimit > 0 {
		opts.memberLimit = h.MemberLimit
	}
	if h.NoSremLimit > 0 {
		opts.noSremLimit = h.NoSremLimit
	}
	opts.beforeSrem = h.BeforeSrem
	opts.secondCheck = h.SecondCheck
	return auditApps(ctx, c, opts)
}
