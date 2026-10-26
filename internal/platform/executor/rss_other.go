//go:build !linux

package executor

// SamplesMemory reports whether LeaderRSS can sample on this system.
const SamplesMemory = false

// LeaderRSS returns ErrUnsupported outside Linux: there is no /proc to
// sample.
func (e *Executor) LeaderRSS(Group) (uint64, error) { return 0, ErrUnsupported }
