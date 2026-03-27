package hydrator

import "testing"

func TestClassifyPriority(t *testing.T) {
	if got := ClassifyPriority("README.md"); got < PriorityBootstrap {
		t.Fatalf("README should be boosted, got %d", got)
	}
	if got := ClassifyPriority("src/main.go"); got < PriorityLikelyText {
		t.Fatalf("go file should be likely text, got %d", got)
	}
	if got := ClassifyPriority("assets/logo.png"); got > PriorityBinary {
		t.Fatalf("png should be penalized, got %d", got)
	}
}
