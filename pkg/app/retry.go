package app

import "time"

// The retry budget and backoff of C-33.
const (
	// MaxAttempts is the number of failed attempts one installation spends
	// before it is declared Failed.
	MaxAttempts = 5
	// InitialBackoff is the wait before the second attempt; each further
	// attempt doubles it.
	InitialBackoff = 10 * time.Second
	// MaxBackoff caps the wait between two attempts.
	MaxBackoff = 5 * time.Minute
	// UninstallStallWindow is how long an uninstall may stay blocked before it
	// is reported as stalled.
	UninstallStallWindow = 10 * time.Minute
)

// Backoff is the wait before the next attempt after failedAttempts failures:
// ten seconds, doubling, capped at five minutes (C-33).
func Backoff(failedAttempts int) time.Duration {
	if failedAttempts < 1 {
		failedAttempts = 1
	}
	wait := InitialBackoff
	for attempt := 1; attempt < failedAttempts; attempt++ {
		wait *= 2
		if wait >= MaxBackoff {
			return MaxBackoff
		}
	}
	return min(wait, MaxBackoff)
}
