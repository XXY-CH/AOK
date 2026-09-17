//go:build !linux

package runtime

import "path/filepath"

// engineAddr returns the child engine listener address.
//
// Only Linux has an abstract Unix socket namespace. Elsewhere (Darwin, BSD) a
// leading "@" is an ordinary filename, and Go's unixSocket bind skips unlinking
// names it reads as abstract, so such a listener leaks a socket file into the
// process working directory. Bind inside the per-child temp directory instead:
// reapLocked removes that directory, so the socket cannot outlive the child.
func engineAddr(dir string) string { return filepath.Join(dir, "engine.sock") }
