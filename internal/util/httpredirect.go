package util

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// CheckSameHostRedirect is the shared upstream redirect policy: follow
// same-host hops (http→https or a path move) and refuse CROSS-HOST
// redirects. Go's client only strips Authorization/Cookie on a cross-host
// hop; a custom auth_header (e.g. "x-api-key") would be forwarded to the
// redirect target verbatim. Used by the account HTTP client and by the
// quota CLI fetch path (AccountView.Client() is nil there).
func CheckSameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) == 0 || req == nil || req.URL == nil {
		return nil
	}
	// via[0] is the ORIGINAL request (Go passes the full chain): its host
	// is the account's configured base URL host. Hosts are compared
	// case-insensitively and port-less (Hostname), so
	// "api.example.com:443" → "api.example.com" is the same host.
	orig := via[0].URL.Hostname()
	next := req.URL.Hostname()
	if !strings.EqualFold(orig, next) {
		return fmt.Errorf("refusing cross-host redirect to %q (account base host %q): account headers/credentials must never leave the configured host", next, orig)
	}
	return nil
}
