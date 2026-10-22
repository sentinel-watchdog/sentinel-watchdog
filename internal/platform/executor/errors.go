package executor

import "errors"

// ErrUnsupported is returned for what only Linux provides, such as an
// identity change or a process group record: sentineld runs processes on
// Linux only (D-084); other systems are for development.
var ErrUnsupported = errors.New("executor: supported on Linux only")

// ErrCleanupIncomplete is returned by Cleanup when it cannot tell that a
// recorded group is gone: /proc could not be read completely, or members
// were still alive after the last round.
var ErrCleanupIncomplete = errors.New("executor: the recorded process group could not be cleaned up")
