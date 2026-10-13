// Package shutdown lets live tell sse that a stream's context ends
// because the hub stops, while the client is still there.
package shutdown

import (
	"context"
	"fmt"
)

// Cause is the cause of a stream's context when the hub stops. The stream
// then ends cleanly where it can: a write under way still fails at once,
// but Close sends the end of the compressed stream, and the response ends
// normally. It wraps context.Canceled.
var Cause = fmt.Errorf("lith: shutting down: %w", context.Canceled)
