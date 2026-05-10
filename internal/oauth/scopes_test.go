package oauth

import "testing"

// TestScopes_OnlyReadonly is the load-bearing regression test for the
// read-only edition of msgvault. It asserts that the requested OAuth
// scope set contains exactly one scope: gmail.readonly.
//
// Why this test exists, even though the scope list is right there in
// oauth.go: a future "let me add labels support" or "let me ship a
// trash command" PR could silently widen Scopes by appending another
// string, and the compiler would not flag it (Scopes is []string —
// any string is type-valid). This test is the brake.
//
// The contract is also user-visible: a Google Cloud project whose
// OAuth consent screen lists only gmail.readonly must complete OAuth
// against this binary without an `invalid_scope` error. Widening the
// scope list here would break that contract for those users.
//
// If you intentionally want to add a scope, change this test in the
// same PR and document why in the commit message and in
// plans/readonly-conversion.md.
func TestScopes_OnlyReadonly(t *testing.T) {
	const want = "https://www.googleapis.com/auth/gmail.readonly"

	if got := len(Scopes); got != 1 {
		t.Fatalf("len(Scopes) = %d, want 1; this is the read-only "+
			"edition and must request only gmail.readonly", got)
	}
	if Scopes[0] != want {
		t.Errorf("Scopes[0] = %q, want %q", Scopes[0], want)
	}
}
