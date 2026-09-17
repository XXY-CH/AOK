//go:build linux

package runtime

import "path/filepath"

// engineAddr returns the child engine listener address.
//
// Linux has an abstract Unix socket namespace: a name starting with NUL (spelled
// "@" by Go) is not a filesystem path, so it stays addressable after the parent
// closes its own listener copy and leaves nothing to unlink.
func engineAddr(dir string) string { return "@aok-" + filepath.Base(dir) }
