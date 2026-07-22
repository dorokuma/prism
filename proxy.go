package main

import (
	"net/http"

	"github.com/dorokuma/prism/internal/config"
	"github.com/dorokuma/prism/internal/proxy"
	"github.com/dorokuma/prism/internal/sanitize"
	"github.com/dorokuma/prism/internal/util"
)

// OpenAIErrorResponse represents an OpenAI API error response body.
type OpenAIErrorResponse = proxy.OpenAIErrorResponse

// NewProxyHandler creates the main HTTP handler.
func NewProxyHandler(pool *Pool, wire WireAPIMode, holder *ConfigHolder) http.Handler {
	return proxy.NewProxyHandler(pool, config.WireAPIMode(wire), holder)
}

// isPermanentCredentialError checks if the response body indicates a permanent credential error.
var isPermanentCredentialError = proxy.IsPermanentCredentialError

// isQuotaError checks if the response body indicates a quota/rate-limit error.
var isQuotaError = proxy.IsQuotaError

// redactBody shims — used by main.go and tests.
var redactBody = util.RedactBody
var redactBodyBytes = util.RedactBodyBytes
var redactBodyRegex = util.RedactBodyRegex
var redactJSON = util.RedactJSON
var redactBodyBytesWithKeys = util.RedactBodyBytesWithKeys
var redactJSONWithKeys = util.RedactJSONWithKeys
var redactStringWithKeys = util.RedactStringWithKeys

// chatForwardOpts type alias — exported for tests.
type chatForwardOpts = proxy.ChatForwardOpts

// proxyChatWithBody shim — used by logging tests.
var proxyChatWithBody = proxy.ProxyChatWithBody

// transformRequestBody shim — used by proxy package.
var transformRequestBody = sanitize.TransformRequestBody
