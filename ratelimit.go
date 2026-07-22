package main

import "github.com/dorokuma/prism/internal/ratelimit"

// rateLimiter is a re-export for backward compatibility.
type rateLimiter = ratelimit.RateLimiter

// newRateLimiter is a re-export for backward compatibility.
var newRateLimiter = ratelimit.NewRateLimiter

// rateLimitMiddleware is a re-export for backward compatibility.
var rateLimitMiddleware = ratelimit.RateLimitMiddleware

// getClientIP is a re-export for backward compatibility.
var getClientIP = ratelimit.GetClientIP
