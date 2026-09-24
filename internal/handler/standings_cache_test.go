package handler

import "testing"

func TestRankAgainst(t *testing.T) {
	// score DESC, id ASC — the order roomSnapshot reads in.
	rows := []StandingRow{
		{ID: 4, Score: 300}, {ID: 2, Score: 200}, {ID: 7, Score: 200}, {ID: 1, Score: 100}, {ID: 9, Score: 0},
	}
	cases := []struct {
		name  string
		score int
		id    uint
		want  int
	}{
		{"leader", 300, 4, 1},
		{"tie broken by lower id", 200, 2, 2},
		{"tie, higher id", 200, 7, 3},
		{"new score above everyone", 500, 9, 1},
		{"stale own row ignored", 250, 1, 2}, // snapshot still has id 1 at 100
		{"last", 0, 9, 5},
		{"not in snapshot yet", 0, 10, 6},
	}
	for _, c := range cases {
		if got := rankAgainst(rows, c.score, c.id); got != c.want {
			t.Errorf("%s: rankAgainst(%d, %d) = %d, want %d", c.name, c.score, c.id, got, c.want)
		}
	}
	if got := rankAgainst(nil, 10, 1); got != 1 {
		t.Errorf("empty snapshot: got %d, want 1", got)
	}
}
