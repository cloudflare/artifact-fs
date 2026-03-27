package auth

import (
	"net/url"
	"regexp"
	"strings"
)

var tokenLike = regexp.MustCompile(`(?i)(access_token|token|password|passwd|secret|key|authorization|x-token-auth)=([^&\s]+)`)

func RedactRemoteURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return tokenLike.ReplaceAllString(raw, `$1=REDACTED`)
	}
	if u.User != nil {
		username := u.User.Username()
		if _, ok := u.User.Password(); ok || username != "" {
			u.User = url.User("REDACTED")
		}
	}
	if u.RawQuery != "" {
		u.RawQuery = tokenLike.ReplaceAllString(u.RawQuery, `$1=REDACTED`)
	}
	return u.String()
}

func RedactString(s string) string {
	if s == "" {
		return ""
	}
	s = tokenLike.ReplaceAllString(s, `$1=REDACTED`)
	// Redact any URL-shaped substring with credentials (not just those with @)
	if strings.Contains(s, "://") {
		parts := strings.Split(s, " ")
		for i := range parts {
			if strings.Contains(parts[i], "://") {
				parts[i] = RedactRemoteURL(parts[i])
			}
		}
		s = strings.Join(parts, " ")
	}
	return s
}
