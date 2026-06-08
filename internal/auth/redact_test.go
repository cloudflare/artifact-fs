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
}

func TestHasInlineCredentials(t *testing.T) {
	if !HasInlineCredentials("https://token@example.com/org/repo.git") {
		t.Fatal("expected inline credentials")
	}
	if !HasInlineCredentials("https://github.com/org/repo.git?access_token=secret") {
		t.Fatal("expected token query parameter credentials")
	}
	if !HasInlineCredentials("https://github.com/org/repo.git?x-token-auth=secret") {
		t.Fatal("expected token-like query parameter credentials")
	}
	if HasInlineCredentials("git@github.com:org/repo.git") {
		t.Fatal("scp-style SSH remote should not count as inline credentials")
	}
	if HasInlineCredentials("ssh://git@github.com/org/repo.git") {
		t.Fatal("standard SSH URL username should use ambient auth, not count as inline credentials")
	}
	if !HasInlineCredentials("ssh://git:secret@github.com/org/repo.git") {
		t.Fatal("expected SSH URL password to count as inline credentials")
	}
	if HasInlineCredentials("https://github.com/org/repo.git?ref=main") {
		t.Fatal("unexpected credentials in benign query")
	}
	if HasInlineCredentials("https://github.com/org/repo.git") {
		t.Fatal("unexpected inline credentials")
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
