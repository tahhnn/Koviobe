package license

import (
	"log"

	"github.com/quizzzone/backend/internal/db"
)

// Blocker and warning identifiers. Protocol values the admin console maps to
// text, never prose — they are also what the audit log and the API expose.
const (
	BlockerFreePlanUnlocked = "free_plan_unlocked"
	BlockerAdminsWithoutPro = "admins_without_pro"

	WarnActiveHostsOnFree  = "active_hosts_on_free"
	WarnLiveRoomsAtRisk    = "live_rooms_at_risk"
	WarnGrandfatherMissing = "grandfather_missing"
)

// AdminWithoutPro names an admin who would lose their own host capability the
// moment enforcement is switched on.
type AdminWithoutPro struct {
	UserID uint   `json:"user_id"`
	Email  string `json:"email"`
}

// EnforcementReadiness is the pre-flight report shown before the switch is
// thrown. Blockers refuse the change (unless forced); warnings only inform.
type EnforcementReadiness struct {
	Ready             bool              `json:"ready"`
	FreePlanLocked    bool              `json:"free_plan_locked"`
	GrandfatherRan    bool              `json:"grandfather_ran"`
	AdminsWithoutPro  []AdminWithoutPro `json:"admins_without_pro"`
	ActiveHostsOnFree int64             `json:"active_hosts_on_free"`
	TotalActiveHosts  int64             `json:"total_active_hosts"`
	LiveRoomsAtRisk   int64             `json:"live_rooms_at_risk"`
	Blockers          []string          `json:"blockers"`
	Warnings          []string          `json:"warnings"`
}

// activeHostPredicate is the "already using the product" test, copied verbatim
// from scripts/license/02_grandfather_hosts.sql.
//
// Kept identical on purpose: the number this check shows and the blast radius of
// the script the operator is told to run must agree, or the check teaches them
// to distrust it.
const activeHostPredicate = `users.deleted_at IS NULL AND (
	roles.name = 'admin'
	OR EXISTS (SELECT 1 FROM quizzes q   WHERE q.host_id = users.id AND q.deleted_at IS NULL)
	OR EXISTS (SELECT 1 FROM templates t WHERE t.host_id = users.id AND t.deleted_at IS NULL)
	OR EXISTS (SELECT 1 FROM rooms rm    WHERE rm.host_id = users.id AND rm.deleted_at IS NULL)
)`

// noValidPaidSub is true for a user with no subscription row, a free one, an
// inactive one, or one whose term has already lapsed.
const noValidPaidSub = `(subscriptions.user_id IS NULL
	OR subscriptions.plan_id = 'free'
	OR subscriptions.status <> 'active'
	OR (subscriptions.ends_at IS NOT NULL AND subscriptions.ends_at < NOW()))`

// CheckEnforcementReadiness reports whether enforcement can be switched on
// without locking the people who run the product out of it.
//
// It never writes. In particular it does not call GetEntitlements, which creates
// free subscriptions and downgrades expired ones on its miss paths — a readiness
// check that causes the problem it reports is worse than no check.
func CheckEnforcementReadiness() EnforcementReadiness {
	r := EnforcementReadiness{
		AdminsWithoutPro: []AdminWithoutPro{},
		Blockers:         []string{},
		Warnings:         []string{},
	}
	if db.DB == nil {
		r.Blockers = append(r.Blockers, BlockerFreePlanUnlocked)
		return r
	}

	// 1. Is the free plan actually locked down? (the automated form of
	// 01_lock_free_plan.sql having been run)
	var freeLimits struct {
		MaxPlayersPerRoom   int
		MaxQuizzes          int
		MaxTemplates        int
		MaxConcurrentRooms  int
		MaxQuestionsPerQuiz int
	}
	err := db.DB.Table("pricing_plans").
		Select("max_players_per_room, max_quizzes, max_templates, max_concurrent_rooms, max_questions_per_quiz").
		Where("id = ? AND deleted_at IS NULL", PlanFree).
		Scan(&freeLimits).Error
	if err != nil {
		log.Printf("[LICENSE] readiness: free plan read failed: %v", err)
	} else {
		r.FreePlanLocked = freeLimits.MaxPlayersPerRoom == 0 &&
			freeLimits.MaxQuizzes == 0 &&
			freeLimits.MaxTemplates == 0 &&
			freeLimits.MaxConcurrentRooms == 0 &&
			freeLimits.MaxQuestionsPerQuiz == 0
	}

	// 2. Admins without a usable paid plan — the lockout that costs the operator
	// their own ability to use the product.
	if err := db.DB.Table("users").
		Select("users.id AS user_id, users.email").
		Joins("JOIN roles ON roles.id = users.role_id").
		Joins("LEFT JOIN subscriptions ON subscriptions.user_id = users.id AND subscriptions.deleted_at IS NULL").
		Where("users.deleted_at IS NULL AND roles.name = 'admin'").
		Where(noValidPaidSub).
		Order("users.id").
		Scan(&r.AdminsWithoutPro).Error; err != nil {
		log.Printf("[LICENSE] readiness: admin check failed: %v", err)
	}

	// 3. Active hosts still unlicensed, and 4. their open rooms.
	// Each count builds its own query rather than reusing a *gorm.DB — the same
	// condition-accumulation trap history.go's scope() closure exists to avoid.
	if err := db.DB.Table("users").
		Joins("LEFT JOIN roles ON roles.id = users.role_id").
		Where(activeHostPredicate).
		Count(&r.TotalActiveHosts).Error; err != nil {
		log.Printf("[LICENSE] readiness: active host count failed: %v", err)
	}
	if err := db.DB.Table("users").
		Joins("LEFT JOIN roles ON roles.id = users.role_id").
		Joins("LEFT JOIN subscriptions ON subscriptions.user_id = users.id AND subscriptions.deleted_at IS NULL").
		Where(activeHostPredicate).
		Where(noValidPaidSub).
		Count(&r.ActiveHostsOnFree).Error; err != nil {
		log.Printf("[LICENSE] readiness: unlicensed host count failed: %v", err)
	}
	if err := db.DB.Table("rooms").
		Joins("JOIN users ON users.id = rooms.host_id").
		Joins("LEFT JOIN roles ON roles.id = users.role_id").
		Joins("LEFT JOIN subscriptions ON subscriptions.user_id = users.id AND subscriptions.deleted_at IS NULL").
		Where("rooms.deleted_at IS NULL AND rooms.status IN ?", []string{"waiting", "active"}).
		Where(activeHostPredicate).
		Where(noValidPaidSub).
		Count(&r.LiveRoomsAtRisk).Error; err != nil {
		log.Printf("[LICENSE] readiness: live room count failed: %v", err)
	}

	// 5. Did the grandfather step ever run? Same signal LICENSING.md prescribes.
	var events int64
	if err := db.DB.Table("subscription_events").Count(&events).Error; err != nil {
		log.Printf("[LICENSE] readiness: event count failed: %v", err)
	}
	r.GrandfatherRan = events > 0

	r.Blockers, r.Warnings = classifyReadiness(
		r.FreePlanLocked, r.GrandfatherRan, len(r.AdminsWithoutPro), r.ActiveHostsOnFree, r.LiveRoomsAtRisk)
	r.Ready = len(r.Blockers) == 0
	return r
}

// classifyReadiness turns raw counts into blockers and warnings.
//
// Split from the queries so the decision — the part that is easy to get wrong
// and impossible to test against a live database — is a pure function.
//
// Hosts still on free is deliberately never a blocker: cutting off unpaid hosts
// is the entire point of the feature.
func classifyReadiness(freePlanLocked, grandfatherRan bool, adminsMissing int, hostsOnFree, liveRooms int64) (blockers, warnings []string) {
	blockers = []string{}
	warnings = []string{}

	if !freePlanLocked {
		blockers = append(blockers, BlockerFreePlanUnlocked)
	}
	if adminsMissing > 0 {
		blockers = append(blockers, BlockerAdminsWithoutPro)
	}
	if hostsOnFree > 0 {
		warnings = append(warnings, WarnActiveHostsOnFree)
	}
	if liveRooms > 0 {
		warnings = append(warnings, WarnLiveRoomsAtRisk)
	}
	if !grandfatherRan {
		warnings = append(warnings, WarnGrandfatherMissing)
	}
	return blockers, warnings
}
