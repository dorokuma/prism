package main

import (
	"github.com/dorokuma/prism/internal/middleware"
	"github.com/dorokuma/prism/internal/util"
)

// ---------------------------------------------------------------------------
// Shim declarations — all moved to internal/middleware or internal/util.
// ---------------------------------------------------------------------------

var requestIDFromCtx = util.RequestIDFromCtx
var requestIDMiddleware = middleware.RequestIDMiddleware

var initLogger = middleware.InitLogger
var setLogLevel = middleware.SetLogLevel
var parseLevel = middleware.ParseLevel

var auditFromCtx = middleware.AuditFromCtx
var emitAudit = middleware.EmitAudit

type requestIDKey = util.RequestIDKey
type requestAudit = middleware.RequestAudit
type auditKey = middleware.AuditKey
type statusCapture = middleware.StatusCapture
