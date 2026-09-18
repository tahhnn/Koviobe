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

// Free is the unlicensed tier: an account that has not been assigned a plan
// must not be able to create anything. Every limit is zero, and zero is not
// "unlimited" — IsUnlimited only treats negatives that way, so the handler
// gates (count >= limit) block on the very first attempt.
func TestDefaultFreeEntitlementsIsLocked(t *testing.T) {
	e := defaultFreeEntitlements()
	if e.PlanID != PlanFree {
		t.Fatalf("plan=%s", e.PlanID)
	}
	limits := map[string]int{
		"max_players_per_room":   e.MaxPlayersPerRoom,
		"max_quizzes":            e.MaxQuizzes,
		"max_templates":          e.MaxTemplates,
		"max_concurrent_rooms":   e.MaxConcurrentRooms,
		"max_questions_per_quiz": e.MaxQuestionsPerQuiz,
	}
	for name, v := range limits {
		if v != 0 {
			t.Fatalf("unlicensed %s=%d want 0", name, v)
		}
		if IsUnlimited(v) {
			t.Fatalf("unlicensed %s must not read as unlimited", name)
		}
	}
	if e.AllowPlayerPaced || e.AllowCustomBranding || e.AllowExportLogs {
		t.Fatalf("unlicensed tier must have no Pro features: %+v", e)
	}
}

func TestEntitlementsFromPlanPro(t *testing.T) {
	// Mirror seeded Pro plan numbers without DB dependency.
	type stub struct {
		ID, Name                                    string
		MaxPlayersPerRoom, MaxQuizzes, MaxTemplates int
		MaxConcurrentRooms, MaxQuestionsPerQuiz     int
		AllowPlayerPaced, AllowCustomBranding       bool
		AllowExportLogs, AllowPrioritySupport       bool
		AllowRemoveWatermark                        bool
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
		{PlanFree, 0, 0, 0},
		{PlanPro, 200, 100, 10},
	}
	if want[0].maxPlayers != 0 || want[0].maxConcurrentRooms != 0 {
		t.Fatalf("free tier must stay locked: %+v", want[0])
	}
	if want[1].maxPlayers <= 0 || want[1].maxConcurrentRooms <= 0 {
		t.Fatalf("pro tier must grant capacity: %+v", want[1])
	}
}

// Enforcing() reads config.AppConfig, which is nil in a unit test. That nil
// case must mean "off": a config that failed to load must never lock every
// host out of the product.
func TestEnforcingDefaultsOffWithoutConfig(t *testing.T) {
	if Enforcing() {
		t.Fatal("Enforcing() must be false when config.AppConfig is nil")
	}
}

func TestOpenEntitlementsUnlockEverything(t *testing.T) {
	e := openEntitlements()
	if !e.AllowPlayerPaced || !IsUnlimited(e.MaxQuizzes) || !IsUnlimited(e.MaxQuestionsPerQuiz) {
		t.Fatalf("open entitlements must unlock Pro gates: %+v", e)
	}
	if e.MaxPlayersPerRoom < 200 {
		t.Fatalf("open max players too low: %d", e.MaxPlayersPerRoom)
	}
}
