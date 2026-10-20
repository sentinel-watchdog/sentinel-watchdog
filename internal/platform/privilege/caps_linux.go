package privilege

import (
	"fmt"
	"os"
)

// Current returns sentineld's effective capabilities.
func Current() (Caps, error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0, fmt.Errorf("read capabilities: %w", err)
	}
	defer f.Close()
	return ParseStatus(f)
}
