package avatar

import "testing"

func TestCodesAreUniqueAndValid(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Codes {
		if seen[c] {
			t.Fatalf("duplicate code %q", c)
		}
		seen[c] = true
		if !Valid(c) {
			t.Fatalf("code %q not valid", c)
		}
		if len(c) > 16 {
			t.Fatalf("code %q longer than the players.avatar column", c)
		}
	}
}

func TestResolve(t *testing.T) {
	if got := Resolve("1f98a"); got != "1f98a" {
		t.Fatalf("valid pick changed: %q", got)
	}
	if got := Resolve("  1F98A "); got != "1f98a" {
		t.Fatalf("case/space not normalised: %q", got)
	}
	for _, in := range []string{"", "🦊", "1f4a9", "<script>", "../../etc"} {
		if got := Resolve(in); !Valid(got) {
			t.Fatalf("Resolve(%q) = %q, want an allowed code", in, got)
		}
	}
}

func TestRandomStaysInSet(t *testing.T) {
	for i := 0; i < 500; i++ {
		if c := Random(); !Valid(c) {
			t.Fatalf("Random returned %q", c)
		}
	}
}
