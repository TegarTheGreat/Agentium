package policy

import "testing"

func TestCheckRules(t *testing.T) {
	good := Rules{Allow: []string{"bash(go test*)", "Edit(src/**)", "read", "mcp(github__*)"}, Deny: []string{"write(.env)"}}
	if err := CheckRules(good); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"bsh(rm*)", "fetch(domain:x.com)", "bash(go test*", "bash()"} {
		if CheckRules(Rules{Deny: []string{bad}}) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
