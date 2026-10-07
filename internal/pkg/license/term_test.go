package license

import (
	"testing"
	"time"

	"github.com/quizzzone/backend/internal/model"
)

func days(n int) *int { return &n }

func TestComputeTerm(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	start := now.AddDate(0, 0, -20)
	in10 := now.AddDate(0, 0, 10)
	ago5 := now.AddDate(0, 0, -5)

	proLive := &model.Subscription{PlanID: PlanPro, StartsAt: start, EndsAt: &in10}
	proLapsed := &model.Subscription{PlanID: PlanPro, StartsAt: start, EndsAt: &ago5}
	proLifetime := &model.Subscription{PlanID: PlanPro, StartsAt: start}
	free := &model.Subscription{PlanID: PlanFree, StartsAt: start}

	cases := []struct {
		name      string
		current   *model.Subscription
		plan      string
		opts      AssignOptions
		wantStart time.Time
		wantEnd   *time.Time // nil = lifetime
	}{
		{"renew early with extend keeps the remaining days", proLive, PlanPro,
			AssignOptions{EndsAtDays: days(30), Extend: true}, start, ptr(in10.AddDate(0, 0, 30))},
		{"renew early without extend restarts (old behaviour)", proLive, PlanPro,
			AssignOptions{EndsAtDays: days(30)}, now, ptr(now.AddDate(0, 0, 30))},
		{"lapsed term restarts from now even with extend", proLapsed, PlanPro,
			AssignOptions{EndsAtDays: days(30), Extend: true}, now, ptr(now.AddDate(0, 0, 30))},
		{"extend never shortens a lifetime licence", proLifetime, PlanPro,
			AssignOptions{EndsAtDays: days(30), Extend: true}, start, nil},
		{"free to pro with extend is a fresh term", free, PlanPro,
			AssignOptions{EndsAtDays: days(30), Extend: true}, now, ptr(now.AddDate(0, 0, 30))},
		{"first grant with no subscription row", nil, PlanPro,
			AssignOptions{EndsAtDays: days(30), Extend: true}, now, ptr(now.AddDate(0, 0, 30))},
		{"lifetime grant", proLive, PlanPro, AssignOptions{Extend: true}, now, nil},
		{"downgrade to free clears the end date", proLive, PlanFree,
			AssignOptions{EndsAtDays: days(30), Extend: true}, now, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotStart, gotEnd := computeTerm(now, tc.current, tc.plan, tc.opts)
			if !gotStart.Equal(tc.wantStart) {
				t.Fatalf("start=%v want %v", gotStart, tc.wantStart)
			}
			switch {
			case tc.wantEnd == nil && gotEnd != nil:
				t.Fatalf("end=%v want lifetime", *gotEnd)
			case tc.wantEnd != nil && gotEnd == nil:
				t.Fatalf("end=lifetime want %v", *tc.wantEnd)
			case tc.wantEnd != nil && !gotEnd.Equal(*tc.wantEnd):
				t.Fatalf("end=%v want %v", *gotEnd, *tc.wantEnd)
			}
		})
	}
}

// Extend must never move an end date earlier than it already was.
func TestComputeTermExtendNeverShortens(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for left := 1; left <= 400; left += 37 {
		end := now.AddDate(0, 0, left)
		cur := &model.Subscription{PlanID: PlanPro, StartsAt: now, EndsAt: &end}
		_, got := computeTerm(now, cur, PlanPro, AssignOptions{EndsAtDays: days(1), Extend: true})
		if got == nil || !got.After(end) {
			t.Fatalf("left=%d: end %v not after %v", left, got, end)
		}
	}
}

func ptr(t time.Time) *time.Time { return &t }
