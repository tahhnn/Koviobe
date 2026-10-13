package license

import "testing"

// A customer who just bought a week must not be told it is running out.
func TestWarnsAtSkipsThresholdsAsLongAsHalfTheTerm(t *testing.T) {
	cases := []struct {
		term      float64
		threshold int
		want      bool
	}{
		{7, 7, false}, {7, 3, true}, {7, 1, true},
		{30, 7, true}, {30, 3, true}, {30, 1, true},
		{365, 7, true},
		{14, 7, false}, {15, 7, true},
		{3, 1, true}, {2, 1, false},
	}
	for _, c := range cases {
		if got := warnsAt(c.term, c.threshold); got != c.want {
			t.Errorf("warnsAt(%v, %d) = %v, want %v", c.term, c.threshold, got, c.want)
		}
	}
}
