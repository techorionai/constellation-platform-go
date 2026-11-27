package valkey

// SetAuditMemberLimit sets the scan limit of AuditApps. It returns a function
// that restores the old value. Only tests use it.
func SetAuditMemberLimit(n int) (restore func()) {
	old := auditMemberLimit
	auditMemberLimit = n
	return func() { auditMemberLimit = old }
}

// SetAuditBeforeSrem sets a hook that runs before the SREM of a page. It returns
// a function that removes the hook.
func SetAuditBeforeSrem(f func()) (restore func()) {
	auditBeforeSrem = f
	return func() { auditBeforeSrem = nil }
}

// SetAuditSecondCheck sets a hook that runs before each second check. It returns
// a function that removes the hook.
func SetAuditSecondCheck(f func()) (restore func()) {
	auditSecondCheck = f
	return func() { auditSecondCheck = nil }
}
