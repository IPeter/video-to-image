package pipeline

import "time"

// CleanupTimeout exposes cleanupTimeout to the tests.
const CleanupTimeout = cleanupTimeout

// SetPersistBackoff replaces the waits between persist retries (nil → the default;
// empty → a single attempt), so tests of the retry paths run fast.
func SetPersistBackoff(r *Runner, backoff []time.Duration) { r.persistBackoff = backoff }
