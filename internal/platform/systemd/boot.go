package systemd

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// bootIDPath changes at every boot (random(4)).
const bootIDPath = "/proc/sys/kernel/random/boot_id"

var bootRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// BootID returns the id of the running boot: a unit's InvocationID
// changes at every boot, which is not a restart of the unit. It fails
// where the kernel has no boot id (not Linux) or gives a malformed one.
func BootID() (string, error) {
	b, err := os.ReadFile(bootIDPath)
	if err != nil {
		return "", fmt.Errorf("read the boot id: %w", err)
	}
	id := strings.TrimSpace(string(b))
	if !bootRe.MatchString(id) {
		return "", errors.New("read the boot id: not a boot id")
	}
	return id, nil
}
