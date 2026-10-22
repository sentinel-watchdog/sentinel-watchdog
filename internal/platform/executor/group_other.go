//go:build !linux

package executor

import "context"

// bootID is empty outside Linux: no group is recorded.
func bootID() (string, error) { return "", nil }

// captureGroup records nothing outside Linux.
func (e *Executor) captureGroup(int) (Group, error) { return Group{}, nil }

// Cleanup has nothing to do for an empty record; any other record was
// written on Linux and cannot be checked here.
func (e *Executor) Cleanup(_ context.Context, g Group) error {
	if g.IsZero() {
		return nil
	}
	return ErrUnsupported
}
