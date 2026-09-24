package studiojob

import "time"

// SetPersistBackoff shortens the retries of persist writes (tests).
func SetPersistBackoff(r *Runner, b []time.Duration) { r.persistBackoff = b }
