package jwt

import "time"

// SetTick changes the real-time poll interval of the background refresh. Call it
// before Start.
func (v *Verifier) SetTick(d time.Duration) { v.tick = d }
