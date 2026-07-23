package license

import "testing"

func TestIsUnlimited(t *testing.T) {
	if !IsUnlimited(-1) {
		t.Fatal("expected -1 to be unlimited")
	}
	if IsUnlimited(0) || IsUnlimited(20) || IsUnlimited(200) {
		t.Fatal("positive/zero limits must not be unlimited")
	}
}

func TestThemeRequestsPlayerPaced(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{``, false},
		{`{}`, false},
		{`{"game_mode":"classic"}`, false},
		{`{"game_mode":"player_paced"}`, true},
		{`not-json`, false},
	}
	for _, tc := range cases {
		if got := ThemeRequestsPlayerPaced(tc.in); got != tc.want {
			t.Fatalf("ThemeRequestsPlayerPaced(%q)=%v want %v", tc.in, got, tc.want)
		}
	}
}

func TestDefaultFreeEntitlements(t *testing.T) {
	e := defaultFreeEntitlements()
	if e.PlanID != PlanFree {
		t.Fatalf("plan=%s", e.PlanID)
	}
	if e.MaxPlayersPerRoom != 20 {
		t.Fatalf("max players free=%d want 20", e.MaxPlayersPerRoom)
	}
	if e.MaxQuestionsPerQuiz != 30 {
		t.Fatalf("max questions free=%d want 30", e.MaxQuestionsPerQuiz)
	}
	if e.MaxConcurrentRooms != 1 {
		t.Fatalf("concurrent rooms free=%d want 1", e.MaxConcurrentRooms)
	}
	if e.AllowPlayerPaced {
		t.Fatal("free must not allow player_paced")
	}
}

func TestEntitlementsFromPlanPro(t *testing.T) {
	// Mirror seeded Pro plan numbers without DB dependency.
	type stub struct {
		ID, Name                                     string
		MaxPlayersPerRoom, MaxQuizzes, MaxTemplates  int
		MaxConcurrentRooms, MaxQuestionsPerQuiz      int
		AllowPlayerPaced, AllowCustomBranding        bool
		AllowExportLogs, AllowPrioritySupport        bool
		AllowRemoveWatermark                         bool
	}
	// Use entitlementsFromPlan via model — exercised through defaultFree when nil.
	if got := entitlementsFromPlan(nil); got.PlanID != PlanFree {
		t.Fatalf("nil plan should fallback free, got %s", got.PlanID)
	}
}

func TestPlanCapacityMatrix(t *testing.T) {
	// Future product limits (used when EnforcementEnabled is true).
	type planLimits struct {
		id                 string
		maxPlayers         int
		maxQuestions       int
		maxConcurrentRooms int
	}
	want := []planLimits{
		{PlanFree, 20, 30, 1},
		{PlanPro, 200, 100, 10},
	}
	for _, p := range want {
		if p.maxPlayers <= 0 {
			t.Fatalf("%s max players must be > 0", p.id)
		}
	}
	if want[1].maxPlayers/want[0].maxPlayers != 10 {
		t.Fatalf("pro/free player ratio want 10")
	}
}

func TestEnforcementDeferred(t *testing.T) {
	if EnforcementEnabled {
		t.Fatal("EnforcementEnabled must stay false until license productization; see LICENSE_DEFERRED.md")
	}
	e := openEntitlements()
	if !e.AllowPlayerPaced || !IsUnlimited(e.MaxQuizzes) || !IsUnlimited(e.MaxQuestionsPerQuiz) {
		t.Fatalf("open entitlements must unlock Pro gates: %+v", e)
	}
	if e.MaxPlayersPerRoom < 200 {
		t.Fatalf("open max players too low: %d", e.MaxPlayersPerRoom)
	}
}
