package core

import "log/slog"

// Tracer starts a span for op, with attrs, and returns the function that ends
// it with the operation's error, nil when it succeeded. A journal opened
// without a tracer traces nothing.
//
// The journal calls a tracer, and the end function it returns, with no
// journal lock held. Neither may block, and neither may call back into the
// journal.
//
// decision: spans take no request context. RFC 1's operations carry none, so
// a journal span is a root, correlated with the request that caused it
// through its attributes (journal, share tag, file). Parenting journal spans
// under request spans would need context-taking variants of the calls, not a
// change to the RFC's signatures.
type Tracer func(op SpanOp, attrs ...slog.Attr) (end func(err error))

// SpanOp names the operation a span covers. Its values are the span names
// operators see, so none carries the journal's version.
type SpanOp string

// The operations the journal traces.
const (
	SpanOpen           SpanOp = "open"
	SpanRecoveryPhase  SpanOp = "recovery_phase"
	SpanSync           SpanOp = "sync"
	SpanOffloadPass    SpanOp = "offload_pass"
	SpanRepackPass     SpanOp = "repack_pass"
	SpanFill           SpanOp = "fill"
	SpanReserveRefused SpanOp = "reserve_refused"
	SpanSeal           SpanOp = "seal"
	SpanRetire         SpanOp = "retire"
)
