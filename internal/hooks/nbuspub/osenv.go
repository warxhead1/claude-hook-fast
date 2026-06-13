package nbuspub

import "os"

// osEnviron is a thin indirection over os.Environ to keep Publish
// testable without polluting the real env. Production callers get the
// real environment; tests can shadow this variable.
var osEnviron = func() []string { return os.Environ() }
