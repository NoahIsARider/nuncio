package caps

import "testing"

func TestDeclareAllKinds(t *testing.T) {
	for _, kind := range Kinds() {
		c, err := Declare(kind)
		if err != nil {
			t.Fatalf("Declare(%s): %v", kind, err)
		}
		if !c.GitPush {
			t.Errorf("%s: every supported forge must accept pushes", kind)
		}
		if len(c.AuthPush) == 0 {
			t.Errorf("%s: no push auth declared", kind)
		}
	}
	if _, err := Declare("bogus"); err == nil {
		t.Error("unknown kind accepted")
	}
}

func TestReservedNamespaces(t *testing.T) {
	cases := []struct {
		kind string
		ref  string
		want bool
	}{
		{"github", "refs/pull/12/head", true},
		{"github", "refs/heads/pull/12", false},
		{"gitlab", "refs/merge-requests/7/head", true},
		{"gitlab", "refs/pull/7/head", false},
		{"forgejo", "refs/pull/1/head", true},
		{"tangled", "refs/cobs/x", true},
		{"tangled", "refs/cob-checkpoints/x", true},
		{"tangled", "refs/hidden/x", true},
		{"tangled", "refs/atproto/x", true},
		{"tangled", "refs/heads/main", false},
		{"sourcehut", "refs/anything", false},
	}
	for _, tc := range cases {
		c, err := Declare(tc.kind)
		if err != nil {
			t.Fatal(err)
		}
		glob, got := c.Reserved(tc.ref)
		if got != tc.want {
			t.Errorf("%s %s: reserved=%v (glob %q), want %v", tc.kind, tc.ref, got, glob, tc.want)
		}
	}
}

func TestMatchGlob(t *testing.T) {
	cases := []struct {
		pattern, ref string
		want         bool
	}{
		{"refs/pull/*", "refs/pull/1/head", true},
		{"refs/pull/*", "refs/pull/", true},
		{"refs/pull/*", "refs/pulls/1", false},
		{"refs/heads/*", "refs/heads/a/b", true},
		{"refs/heads/main", "refs/heads/main", true},
		{"refs/heads/*.x", "refs/heads/a.x", true},
		{"refs/*/main", "refs/heads/main", true},
		{"*", "refs/anything", true},
		{"", "refs/heads/main", false},
	}
	for _, tc := range cases {
		if got := MatchGlob(tc.pattern, tc.ref); got != tc.want {
			t.Errorf("MatchGlob(%q,%q)=%v want %v", tc.pattern, tc.ref, got, tc.want)
		}
	}
}

func TestSpecificityOrder(t *testing.T) {
	got := SortPatterns([]string{"refs/*", "refs/heads/*", "refs/tags/*", "refs/heads/main"})
	if got[0] != "refs/heads/main" {
		t.Errorf("most specific = %q, want refs/heads/main (%v)", got[0], got)
	}
}
