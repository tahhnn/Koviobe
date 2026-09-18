package license

import (
	"strings"
	"testing"

	"github.com/quizzzone/backend/internal/model"
)

// The action on a history row is what a report groups by, so the classification
// has to hold for every transition, not just the happy one.
func TestEventActionClassification(t *testing.T) {
	cases := []struct {
		name         string
		previousPlan string
		newPlan      string
		source       string
		want         string
	}{
		{"first grant to a new user", "", PlanPro, SourceCodeRedeem, "grant"},
		{"free user buys pro", PlanFree, PlanPro, SourceCodeRedeem, "grant"},
		{"pro extends pro", PlanPro, PlanPro, SourceCodeRedeem, "renew"},
		{"admin revokes to free", PlanPro, PlanFree, SourceAdminAssign, "downgrade"},
		{"plan lapses", PlanPro, PlanFree, SourceExpiryAuto, "expire"},
		{"already free, granted free again", PlanFree, PlanFree, SourceAdminAssign, "renew"},
	}

	for _, tc := range cases {
		got := classifyAction(tc.previousPlan, tc.newPlan, tc.source)
		if got != tc.want {
			t.Fatalf("%s: previous=%q new=%q source=%q -> %q, want %q",
				tc.name, tc.previousPlan, tc.newPlan, tc.source, got, tc.want)
		}
	}
}

// Money belongs to grants. Counting a downgrade or an expiry would net real
// revenue away, so those actions must never be in the revenue scope.
func TestRevenueCountsOnlyGrantAndRenew(t *testing.T) {
	for _, action := range []string{"downgrade", "expire"} {
		if isRevenueAction(action) {
			t.Fatalf("%q must not count toward revenue", action)
		}
	}
	for _, action := range []string{"grant", "renew"} {
		if !isRevenueAction(action) {
			t.Fatalf("%q must count toward revenue", action)
		}
	}
}

// The mail goes to a customer and interpolates admin-entered text. Anything
// unescaped there is markup the buyer's mail client will render.
func TestCodeEmailEscapesUntrustedText(t *testing.T) {
	codes := []model.LicenseCode{
		{Code: "KOVIO-23AB-45CD-67EF", PlanID: "pro", DurationDays: 30, MaxUses: 1},
	}
	html := codeEmailHTML(codes, `<img src=x onerror="alert(1)">`)

	if strings.Contains(html, "<img src=x") {
		t.Fatal("buyer name was interpolated as raw markup")
	}
	if !strings.Contains(html, "&lt;img") {
		t.Fatalf("buyer name was not escaped: %s", html)
	}
	if !strings.Contains(html, "KOVIO-23AB-45CD-67EF") {
		t.Fatal("the code itself must still appear in the mail")
	}
}

func TestCodeEmailDescribesTerm(t *testing.T) {
	lifetime := codeEmailHTML([]model.LicenseCode{
		{Code: "KOVIO-23AB-45CD-67EF", PlanID: "pro", DurationDays: 0, MaxUses: 1},
	}, "")
	if !strings.Contains(lifetime, "vĩnh viễn") {
		t.Fatal("a 0-day code must be described as lifetime, not as 0 days")
	}

	timed := codeEmailHTML([]model.LicenseCode{
		{Code: "KOVIO-23AB-45CD-67EF", PlanID: "pro", DurationDays: 90, MaxUses: 3},
	}, "")
	if !strings.Contains(timed, "90 ngày") {
		t.Fatal("a timed code must state its term")
	}
	if !strings.Contains(timed, "3 lượt") {
		t.Fatal("a multi-use code must state its use count")
	}
}
