package util

import "strings"

// JoinURLPath joins a base URL with an endpoint path, avoiding duplicate /v1 prefixes or double slashes.
func JoinURLPath(baseURL, endpoint string) string {
	base := strings.TrimSuffix(baseURL, "/")
	ep := strings.TrimPrefix(endpoint, "/")

	if (strings.HasSuffix(base, "/v1") || strings.HasSuffix(base, "/v1/")) && (strings.HasPrefix(ep, "v1/") || ep == "v1") {
		if ep == "v1" {
			ep = ""
		} else {
			ep = strings.TrimPrefix(ep, "v1/")
		}
	}

	if ep == "" {
		return base
	}
	return base + "/" + ep
}
