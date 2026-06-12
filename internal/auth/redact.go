package auth

import (
	"net"
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
		return redactMalformedURL(raw)
	}
	if u.User == nil && strings.Contains(raw, "@") && isMalformedHTTPUserinfo(raw, u) {
		return redactMalformedURL(raw)
	}
	if u.User != nil {
		username := u.User.Username()
		if _, ok := u.User.Password(); ok || username != "" {
			u.User = url.User("REDACTED")
		}
	}
	if u.RawQuery != "" || u.ForceQuery {
		u.RawQuery = "REDACTED"
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		u.Fragment = "REDACTED"
	}
	return u.String()
}

func redactMalformedURL(raw string) string {
	redacted := raw
	authorityStart := malformedAuthorityStart(redacted)
	if authorityStart < 0 {
		return redactQueryFragment(tokenLike.ReplaceAllString(redacted, `$1=REDACTED`))
	}
	userinfoEnd := len(redacted)
	if relEnd := strings.IndexAny(redacted[authorityStart:], "?#"); relEnd >= 0 {
		userinfoEnd = authorityStart + relEnd
	}
	if at := strings.LastIndex(redacted[authorityStart:userinfoEnd], "@"); at >= 0 {
		redacted = redacted[:authorityStart] + "REDACTED" + redacted[authorityStart+at:]
	} else if userinfoEnd < len(redacted) && strings.Contains(redacted[userinfoEnd:], "@") && redacted[authorityStart:userinfoEnd] != "" && !strings.Contains(redacted[authorityStart:userinfoEnd], "/") {
		redacted = redacted[:authorityStart] + "REDACTED" + redacted[userinfoEnd:]
	}
	redacted = tokenLike.ReplaceAllString(redacted, `$1=REDACTED`)
	return redactQueryFragment(redacted)
}

func redactQueryFragment(raw string) string {
	q := strings.Index(raw, "?")
	f := strings.Index(raw, "#")
	if q >= 0 && (f < 0 || q < f) {
		if f >= 0 {
			return raw[:q+1] + "REDACTED#REDACTED"
		}
		return raw[:q+1] + "REDACTED"
	}
	if f >= 0 {
		return raw[:f+1] + "REDACTED"
	}
	return raw
}

func HasInlineCredentials(raw string) bool {
	if strings.ContainsAny(raw, "?#") {
		return true
	}
	urlLike := strings.Contains(raw, "://")
	u, err := url.Parse(raw)
	if err != nil {
		if malformedAuthorityStart(raw) >= 0 && strings.Contains(raw, "@") {
			return true
		}
		if urlLike && strings.ContainsAny(raw, "@?#") {
			return true
		}
		return tokenLike.MatchString(raw)
	}
	if u.User == nil && strings.Contains(raw, "@") && isMalformedHTTPUserinfo(raw, u) {
		return true
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") {
		return true
	}
	if tokenLike.MatchString(u.RawQuery) {
		return true
	}
	if u.User == nil {
		return false
	}
	username := u.User.Username()
	_, hasPassword := u.User.Password()
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return username != "" || hasPassword
	default:
		return hasPassword
	}
}

func malformedAuthorityStart(raw string) int {
	lower := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.Index(lower, "://"); i >= 0 {
		return i + len("://")
	}
	for _, prefix := range []string{"https://", "http://", "ssh://", "git://", "https:/", "http:/", "ssh:/", "git:/", "https//", "http//", "ssh//", "git//", "https:", "http:", "ssh:", "git:"} {
		if strings.HasPrefix(lower, prefix) {
			return len(prefix)
		}
	}
	return -1
}

func isHTTPRemoteLike(raw string, scheme string) bool {
	switch strings.ToLower(scheme) {
	case "http", "https":
		return true
	}
	return malformedAuthorityStart(raw) >= 0
}

func isMalformedHTTPUserinfo(raw string, u *url.URL) bool {
	if !isHTTPRemoteLike(raw, u.Scheme) {
		return false
	}
	if u.Host == "" {
		return true
	}
	if strings.HasPrefix(u.Path, "/@") {
		return true
	}
	if rawUsernameBeforeDelimiter(raw) {
		return true
	}
	if !rawUserinfoCandidateHasPassword(raw) {
		return false
	}
	pathBeforeAt := u.Path
	if at := strings.Index(pathBeforeAt, "@"); at >= 0 {
		pathBeforeAt = pathBeforeAt[:at]
	}
	if strings.Count(strings.Trim(pathBeforeAt, "/"), "/") > 0 {
		host := u.Hostname()
		if strings.Contains(host, ".") || host == "localhost" || net.ParseIP(host) != nil {
			return false
		}
	}
	return true
}

func rawUsernameBeforeDelimiter(raw string) bool {
	authorityStart := malformedAuthorityStart(raw)
	if authorityStart < 0 {
		return false
	}
	at := strings.LastIndex(raw[authorityStart:], "@")
	if at < 0 {
		return false
	}
	at += authorityStart
	if relEnd := strings.IndexAny(raw[authorityStart:at], "?#"); relEnd >= 0 {
		candidate := raw[authorityStart : authorityStart+relEnd]
		return candidate != "" && !strings.Contains(candidate, "/")
	}
	return false
}

func rawUserinfoCandidateHasPassword(raw string) bool {
	authorityStart := malformedAuthorityStart(raw)
	if authorityStart < 0 {
		return false
	}
	userinfoEnd := len(raw)
	if relEnd := strings.IndexAny(raw[authorityStart:], "?#"); relEnd >= 0 {
		userinfoEnd = authorityStart + relEnd
	}
	at := strings.LastIndex(raw[authorityStart:userinfoEnd], "@")
	if at < 0 {
		return false
	}
	candidate := raw[authorityStart : authorityStart+at]
	return strings.Contains(candidate, ":")
}

func RedactString(s string) string {
	if s == "" {
		return ""
	}
	// Redact any URL-shaped substring with credentials (not just those with @)
	if strings.Contains(s, "://") || strings.ContainsAny(s, "?#@") {
		parts := strings.Split(s, " ")
		for i := range parts {
			if strings.Contains(parts[i], "://") || strings.ContainsAny(parts[i], "?#@") {
				parts[i] = redactRemoteToken(parts[i])
			}
		}
		s = strings.Join(parts, " ")
	}
	s = tokenLike.ReplaceAllString(s, `$1=REDACTED`)
	return s
}

func redactRemoteToken(token string) string {
	start := strings.IndexFunc(token, func(r rune) bool {
		return !strings.ContainsRune("'\"([{<", r)
	})
	if start < 0 {
		return token
	}
	end := strings.LastIndexFunc(token, func(r rune) bool {
		return !strings.ContainsRune("'\")]}>,.:", r)
	})
	if end < start {
		return token
	}
	core := token[start : end+1]
	return token[:start] + redactRemoteCore(core) + token[end+1:]
}

func redactRemoteCore(core string) string {
	var b strings.Builder
	for len(core) > 0 {
		sep := strings.IndexAny(core, ",;")
		if sep < 0 {
			b.WriteString(redactSingleRemoteCore(core))
			break
		}
		next := strings.TrimLeft(core[sep+1:], " ")
		if !hasSeparateRemoteMarker(next) && (separatorInsideUserinfo(core, sep) || !hasRemoteMarker(next)) {
			b.WriteString(redactSingleRemoteCore(core))
			break
		}
		b.WriteString(redactSingleRemoteCore(core[:sep]))
		b.WriteByte(core[sep])
		core = core[sep+1:]
	}
	return b.String()
}

func separatorInsideUserinfo(core string, sep int) bool {
	remoteStart := remoteStartIndex(core[:sep])
	if strings.Contains(core[remoteStart:sep], "@") {
		return false
	}
	authorityStart := authorityStartInCore(core, remoteStart)
	if authorityStart < 0 {
		return false
	}
	candidate := core[authorityStart:sep]
	if strings.Contains(candidate, "/") || candidate == "" {
		return false
	}
	nextBoundary := len(core)
	if relEnd := strings.IndexAny(core[sep+1:], " "); relEnd >= 0 {
		nextBoundary = sep + 1 + relEnd
	}
	return strings.Contains(core[sep+1:nextBoundary], "@")
}

func authorityStartInCore(core string, remoteStart int) int {
	rest := strings.ToLower(core[remoteStart:])
	for _, marker := range []string{"://", ":/", "//", ":"} {
		if i := strings.Index(rest, marker); i >= 0 {
			return remoteStart + i + len(marker)
		}
	}
	return -1
}

func hasSeparateRemoteMarker(s string) bool {
	lower := strings.ToLower(s)
	for _, marker := range []string{"https://", "http://", "ssh://", "git://"} {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	return false
}

func hasRemoteMarker(s string) bool {
	if strings.Contains(s, "://") {
		return true
	}
	lower := strings.ToLower(s)
	for _, marker := range []string{"https:/", "http:/", "ssh:/", "git:/", "https//", "http//", "ssh//", "git//", "https:", "http:", "ssh:", "git:"} {
		if strings.HasPrefix(lower, marker) {
			return true
		}
	}
	return false
}

func redactSingleRemoteCore(core string) string {
	remoteStart := remoteStartIndex(core)
	return core[:remoteStart] + RedactRemoteURL(core[remoteStart:])
}

func remoteStartIndex(s string) int {
	best := -1
	for _, marker := range []string{"https://", "http://", "ssh://", "git://", "https:/", "http:/", "ssh:/", "git:/", "https//", "http//", "ssh//", "git//", "https:", "http:", "ssh:", "git:"} {
		if i := strings.Index(strings.ToLower(s), marker); i >= 0 && (best < 0 || i < best) {
			best = i
		}
	}
	if best >= 0 {
		return best
	}
	if at := strings.Index(s, "@"); at >= 0 && strings.ContainsAny(s[at:], "?#") {
		if eq := strings.LastIndex(s[:at], "="); eq >= 0 {
			return eq + 1
		}
	}
	return 0
}
