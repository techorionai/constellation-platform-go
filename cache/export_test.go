package cache

// SetWaitHook sets a hook that GetOrLoad calls after a call joined the
// singleflight group. Tests use it to know that all callers wait.
func SetWaitHook[T any](c *Cache[T], fn func()) { c.onWait = fn }
