package acp

import "testing"

func TestResolveToolPathKeepsPOSIXCwd(t *testing.T) {
	if got := resolveToolPath("/tmp/work", "notes.txt"); got != "/tmp/work/notes.txt" {
		t.Fatalf("relative = %q, want /tmp/work/notes.txt", got)
	}
	if got := resolveToolPath("/tmp/work", "/etc/hosts"); got != "/etc/hosts" {
		t.Fatalf("absolute = %q, want /etc/hosts", got)
	}
	if got := resolveToolPath("/tmp/work", ""); got != "" {
		t.Fatalf("empty = %q, want empty", got)
	}
}
