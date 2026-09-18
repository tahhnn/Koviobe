package license

import (
	"testing"

	"github.com/quizzzone/backend/internal/model"
)

const testCode = "KOVIO-23AB-45CD-67EF"

func event(source, ref string) *model.SubscriptionEvent {
	return &model.SubscriptionEvent{Source: source, SourceRef: ref}
}

// shouldClawBack is the whole feature: get it wrong in one direction and a code
// revoke takes away a plan somebody paid for, in the other and a refunded
// customer keeps their Pro.
func TestShouldClawBack(t *testing.T) {
	tests := []struct {
		name        string
		currentPlan string
		last        *model.SubscriptionEvent
		want        bool
		wantReason  string
	}{
		{
			name:        "redeemed this code and nothing since",
			currentPlan: PlanPro,
			last:        event(SourceCodeRedeem, testCode),
			want:        true,
		},
		{
			// The case that costs money. A trial code, then a real purchase the
			// admin assigned by hand — revoking the trial code must not touch it.
			name:        "admin granted a plan afterwards",
			currentPlan: PlanPro,
			last:        event(SourceAdminAssign, ""),
			want:        false,
			wantReason:  SourceAdminAssign,
		},
		{
			name:        "redeemed a different code afterwards",
			currentPlan: PlanPro,
			last:        event(SourceCodeRedeem, "KOVIO-9999-8888-7777"),
			want:        false,
			wantReason:  SourceCodeRedeem,
		},
		{
			name:        "term already lapsed and the sweep ran",
			currentPlan: PlanFree,
			last:        event(SourceExpiryAuto, ""),
			want:        false,
			wantReason:  reasonAlreadyFree,
		},
		{
			name:        "second claw-back of the same code is idempotent",
			currentPlan: PlanFree,
			last:        event(SourceAdminRevoke, testCode),
			want:        false,
			wantReason:  reasonAlreadyFree,
		},
		{
			name:        "no history at all",
			currentPlan: PlanPro,
			last:        nil,
			want:        false,
			wantReason:  "no_history",
		},
		{
			name:        "no subscription row",
			currentPlan: "",
			last:        event(SourceCodeRedeem, testCode),
			want:        false,
			wantReason:  reasonAlreadyFree,
		},
		{
			// The ref was stored in whatever shape the redeemer typed. Matching
			// has to survive that, exactly as redemption itself does.
			name:        "source ref written in a non-canonical spelling",
			currentPlan: PlanPro,
			last:        event(SourceCodeRedeem, "kovio 23ab 45cd 67ef"),
			want:        true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := shouldClawBack(testCode, tc.currentPlan, tc.last)
			if got != tc.want {
				t.Fatalf("shouldClawBack = %t, want %t (reason %q)", got, tc.want, reason)
			}
			if !tc.want && reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// A claw-back must file as a downgrade, not a grant. classifyAction reaches that
// verdict by falling through its source check, so pin it: reordering that switch
// would otherwise refile every claw-back as revenue.
func TestClawBackClassifiesAsDowngrade(t *testing.T) {
	if got := classifyAction(PlanPro, PlanFree, SourceAdminRevoke); got != "downgrade" {
		t.Fatalf("classifyAction(pro→free, admin_revoke) = %q, want downgrade", got)
	}
	if isRevenueAction("downgrade") {
		t.Fatal("a downgrade must never count as revenue")
	}
	for _, a := range revenueActions() {
		if a == SourceAdminRevoke {
			t.Fatal("admin_revoke is a source, not an action — it must not appear in revenueActions")
		}
	}
}

func TestOutcomeFor(t *testing.T) {
	if got := outcomeFor(reasonAlreadyFree); got != OutcomeAlreadyFree {
		t.Errorf("outcomeFor(already_free) = %q, want %q", got, OutcomeAlreadyFree)
	}
	if got := outcomeFor(SourceAdminAssign); got != OutcomeSuperseded {
		t.Errorf("outcomeFor(admin_assign) = %q, want %q", got, OutcomeSuperseded)
	}
}
