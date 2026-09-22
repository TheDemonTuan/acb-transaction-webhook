package storage

import (
	"regexp"
	"strings"
)

var storedErrorSensitiveKey = regexp.MustCompile(`(?i)(token|bearer|password|passwd|pwd|secret|cookie|set-cookie|authorization|auth|session(?:id|token)?|dse_[a-z0-9_]*|account(?:number|nbr)?|raw(?:form|html|payload)?)`)

func sanitizeStoredErrorMessage(msg string) string {
	if msg == "" {
		return ""
	}
	lower := strings.ToLower(msg)
	if strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype") || strings.Contains(lower, "<body") {
		return "upstream returned HTML error response"
	}
	if storedErrorSensitiveKey.MatchString(msg) || strings.Contains(lower, "set-cookie:") || strings.Contains(lower, "authorization:") {
		return "sensitive error details redacted"
	}
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
		return "upstream error details redacted"
	}
	const maxLen = 1000
	if len(msg) > maxLen {
		return msg[:maxLen] + "..."
	}
	return msg
}
