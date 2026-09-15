package acb

import "net/url"

// SafePath removes query parameters and fragments before a URL is logged.
func SafePath(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Path == "" {
		return "/"
	}
	return parsed.Path
}
