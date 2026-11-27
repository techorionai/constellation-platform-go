package valkey

import (
	"context"

	valkeygo "github.com/valkey-io/valkey-go"
)

// AuditHooks sets the limit of one call of AuditApps. A zero limit keeps the
// default. Only tests use it.
type AuditHooks struct {
	MemberLimit int
}

// AuditAppsWith runs AuditApps with the limit of h.
func AuditAppsWith(ctx context.Context, c valkeygo.Client, h AuditHooks) ([]string, error) {
	opts := defaultAuditOptions()
	if h.MemberLimit > 0 {
		opts.memberLimit = h.MemberLimit
	}
	return auditApps(ctx, c, opts)
}
