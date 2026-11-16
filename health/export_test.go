package health

import "time"

// WriteTimeout returns the write timeout that NewServer sets for cfg.
func WriteTimeout(cfg Config) time.Duration { return writeTimeout(cfg.withDefaults()) }
