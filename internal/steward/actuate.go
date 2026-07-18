package steward

// Method is the resolved push-actuation mechanism for a session. It is derived
// purely from SessionState by ResolveActuation so both Decide's tests and the
// daemon's executor agree on reachability without any I/O.
type Method int

const (
	// MethodNone — the session cannot be reached by any push path (no pane and
	// no usable PTY socket). Compaction is impossible; the steward can only
	// notify or wait.
	MethodNone Method = iota
	// MethodTmux — inject via `tmux send-keys` into the bound pane. The
	// foundation path: covers all interactive paned sessions, and its failure
	// mode (a mis-sent keystroke) never bricks input.
	MethodTmux
	// MethodPtySock — inject via the daemon bg-pty-host framed socket. The ONLY
	// push path for a headless background/forked worker (no pane exists), gated
	// behind operator permission because the ptyAuth secret is guarded and a
	// wrong-token attach poisons the worker's input until re-key.
	MethodPtySock
)

// String renders the method as the token used in budget.json and logs.
func (m Method) String() string {
	switch m {
	case MethodTmux:
		return "tmux"
	case MethodPtySock:
		return "ptysock"
	default:
		return "none"
	}
}

// ResolveActuation picks the push mechanism for a session, preferring tmux
// because it is broader (all paned sessions) and non-destructive on error. It
// falls back to the PTY socket only for a background worker that has a socket
// and a readable auth secret but no pane — the case where tmux is structurally
// unavailable and the socket is the sole reach.
//
// Precedence: a session that somehow has BOTH a pane and a ptySock still uses
// tmux, because tmux's failure mode is strictly safer.
func ResolveActuation(s SessionState) Method {
	if s.PaneID != "" {
		return MethodTmux
	}
	if s.PtySock != "" && s.PtyAuthAvail {
		return MethodPtySock
	}
	return MethodNone
}
