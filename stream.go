package main

import "github.com/dorokuma/prism/internal/stream"

// streamResponseBody copies the upstream body (SSE) to the client http.ResponseWriter,
// capturing token usage from the tail of the stream for audit logging.
var streamResponseBody = stream.StreamResponseBody
