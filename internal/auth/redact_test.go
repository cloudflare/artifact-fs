package auth

import "testing"

func TestRedactRemoteURL(t *testing.T) {
	in := "https://token123@github.com/org/repo.git?token=abc"
	out := RedactRemoteURL(in)
	if out == in {
		t.Fatalf("expected redaction")
	}
	if containsAny(out, []string{"token123", "abc"}) {
		t.Fatalf("token leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://github.com/org/repo.git?sig=s3cr3t")
	if containsAny(out, []string{"sig", "s3cr3t"}) {
		t.Fatalf("query secret leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://github.com/org/repo.git#access_token=ghp_secret")
	if containsAny(out, []string{"access_token", "ghp_secret"}) {
		t.Fatalf("fragment secret leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret%zz@github.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret", "%zz"}) {
		t.Fatalf("malformed userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user:pa#ss@example.com/org/repo.git")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("malformed userinfo with fragment delimiter leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret#@github.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("malformed username-only userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user:pa/ss@example.com/org/repo.git")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("malformed userinfo with path delimiter leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret%zz:password=abc@example.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret", "%zz", "abc"}) {
		t.Fatalf("malformed userinfo with token-like password leaked in output: %s", out)
	}

	out = RedactRemoteURL("git@github.com:org/repo.git?sig=s3cr3t")
	if containsAny(out, []string{"sig", "s3cr3t"}) {
		t.Fatalf("scp-style query secret leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://ghp_secret/@github.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https:/user:ghp_secret@github.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret", "user"}) {
		t.Fatalf("missing-slash https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user:123/ss@example.com/org/repo.git")
	if containsAny(out, []string{"user", "123", "ss"}) {
		t.Fatalf("numeric-prefix https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user:123/dir/ss@example.com/org/repo.git")
	if containsAny(out, []string{"user", "123", "ss"}) {
		t.Fatalf("multi-segment https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https://user.name:pa/ss@example.com/org/repo.git")
	if containsAny(out, []string{"user.name", "pa", "ss"}) {
		t.Fatalf("dotted malformed https userinfo leaked in output: %s", out)
	}

	out = RedactRemoteURL("https//ghp_secret@github.com/org/repo.git")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("missing-colon https userinfo leaked in output: %s", out)
	}

	out = RedactString("fatal: https://ghp_secret%zz:password=abc@example.com/org/repo.git failed")
	if containsAny(out, []string{"ghp_secret", "%zz", "abc"}) {
		t.Fatalf("redacted string leaked malformed credentials: %s", out)
	}

	out = RedactString("fatal: unable to access 'https://user:pass@example.com/org/repo.git/': failed")
	if containsAny(out, []string{"user", "pass"}) {
		t.Fatalf("quoted URL leaked credentials: %s", out)
	}

	out = RedactString("fatal: git@github.com:org/repo.git?sig=s3cr3t failed")
	if containsAny(out, []string{"sig", "s3cr3t"}) {
		t.Fatalf("scp-style query leaked in redacted string: %s", out)
	}

	out = RedactRemoteURL("ssh://git:sec%zzret@example.com/org/repo.git")
	if containsAny(out, []string{"sec", "%zz", "ret"}) {
		t.Fatalf("malformed ssh userinfo leaked in output: %s", out)
	}

	out = RedactString("fatal: ssh:/git:pa/ss@github.com/org/repo.git failed")
	if containsAny(out, []string{"pa/ss"}) {
		t.Fatalf("malformed ssh-style userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: remote=ssh:/git:pa/ss@github.com/org/repo.git failed")
	if containsAny(out, []string{"pa/ss"}) {
		t.Fatalf("prefixed malformed ssh-style userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https:/user:ghp_secret@github.com/org/repo.git failed")
	if containsAny(out, []string{"ghp_secret", "user"}) {
		t.Fatalf("http-like typo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: remote=https:/user:ghp_secret@github.com/org/repo.git failed")
	if containsAny(out, []string{"ghp_secret", "user"}) {
		t.Fatalf("prefixed http-like typo leaked in redacted string: %s", out)
	}

	out = RedactString("remotes=ssh://git@github.com/org/repo.git,https://ghp_secret@example.com/private.git")
	if containsAny(out, []string{"ghp_secret"}) {
		t.Fatalf("second URL credential leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://github.com/org/repo.git?sig=abc,def failed")
	if containsAny(out, []string{"abc", "def"}) {
		t.Fatalf("query value leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:pa,ss@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("comma-containing userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:pa;http:ss@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("semicolon-containing userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:pa,http:ss?x@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("comma/query malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https://user:pa;http:ss#x@example.com/org/repo.git failed")
	if containsAny(out, []string{"user", "pa", "ss"}) {
		t.Fatalf("semicolon/fragment malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https//ghp_secret,http:ss?x@example.com/org/repo.git failed")
	if containsAny(out, []string{"ghp_secret", "ss"}) {
		t.Fatalf("missing-colon comma/query malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactString("fatal: https//ghp_secret;http:ss#x@example.com/org/repo.git failed")
	if containsAny(out, []string{"ghp_secret", "ss"}) {
		t.Fatalf("missing-colon semicolon/fragment malformed userinfo leaked in redacted string: %s", out)
	}

	out = RedactRemoteURL("https://github.com/%zz?access_token=abc@def")
	if containsAny(out, []string{"abc", "def"}) {
		t.Fatalf("malformed query with at sign leaked in output: %s", out)
	}
}

func TestHasInlineCredentials(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{
			name: "https userinfo",
			raw:  "https://token@example.com/org/repo.git",
			want: true,
		},
		{
			name: "token query parameter",
			raw:  "https://github.com/org/repo.git?access_token=secret",
			want: true,
		},
		{
			name: "token-like query parameter",
			raw:  "https://github.com/org/repo.git?x-token-auth=secret",
			want: true,
		},
		{
			name: "unrecognized query secret",
			raw:  "https://github.com/org/repo.git?sig=s3cr3t",
			want: true,
		},
		{
			name: "benign-looking query",
			raw:  "https://github.com/org/repo.git?ref=main",
			want: true,
		},
		{
			name: "fragment secret",
			raw:  "https://github.com/org/repo.git#access_token=secret",
			want: true,
		},
		{
			name: "empty fragment marker",
			raw:  "https://github.com/org/repo.git#",
			want: true,
		},
		{
			name: "malformed https userinfo path split",
			raw:  "https://ghp_secret/@github.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed https userinfo missing slash",
			raw:  "https:/user:ghp_secret@github.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed https userinfo numeric prefix",
			raw:  "https://user:123/ss@example.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed https userinfo missing colon",
			raw:  "https//ghp_secret@github.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed https parse error missing colon",
			raw:  "https//ghp_secret%zz@github.com/org/repo.git",
			want: true,
		},
		{
			name: "empty query marker",
			raw:  "https://github.com/org/repo.git?",
			want: true,
		},
		{
			name: "malformed userinfo",
			raw:  "https://user%zz@github.com/org/repo.git",
			want: true,
		},
		{
			name: "malformed query secret",
			raw:  "https://github.com/org/%zz.git?sig=s3cr3t",
			want: true,
		},
		{
			name: "scp-style ssh",
			raw:  "git@github.com:org/repo.git",
			want: false,
		},
		{
			name: "scp-style ssh with query",
			raw:  "git@github.com:org/repo.git?sig=s3cr3t",
			want: true,
		},
		{
			name: "scp-style ssh with fragment",
			raw:  "git@github.com:org/repo.git#access_token=secret",
			want: true,
		},
		{
			name: "ssh username only",
			raw:  "ssh://git@github.com/org/repo.git",
			want: false,
		},
		{
			name: "ssh username with query",
			raw:  "ssh://git@github.com/org/repo.git?ref=main",
			want: true,
		},
		{
			name: "ssh password",
			raw:  "ssh://git:secret@github.com/org/repo.git",
			want: true,
		},
		{
			name: "plain https remote",
			raw:  "https://github.com/org/repo.git",
			want: false,
		},
		{
			name: "https path with at sign",
			raw:  "https://git.example.com/team/repo@2026.git",
			want: false,
		},
		{
			name: "https ported path with at sign",
			raw:  "https://git.example.com:8443/team/repo@2026.git",
			want: false,
		},
		{
			name: "https path with colon and at sign",
			raw:  "https://git.example.com/team/repo:v1@2026.git",
			want: false,
		},
		{
			name: "localhost path with colon and at sign",
			raw:  "https://localhost/team/repo:v1@2026.git",
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := HasInlineCredentials(tt.raw); got != tt.want {
				t.Fatalf("HasInlineCredentials(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if n != "" && len(s) >= len(n) && stringContains(s, n) {
			return true
		}
	}
	return false
}

func stringContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
