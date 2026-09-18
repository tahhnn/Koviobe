package license

import (
	"slices"
	"testing"
)

// classifyReadiness is where the decision lives — which conditions refuse the
// switch and which only inform. Getting that wrong either locks the operator out
// of their own product or lets a warning masquerade as a veto.
func TestClassifyReadiness(t *testing.T) {
	tests := []struct {
		name           string
		freePlanLocked bool
		grandfatherRan bool
		adminsMissing  int
		hostsOnFree    int64
		liveRooms      int64
		wantBlockers   []string
		wantWarnings   []string
	}{
		{
			name:           "all clear",
			freePlanLocked: true,
			grandfatherRan: true,
			wantBlockers:   []string{},
			wantWarnings:   []string{},
		},
		{
			name:           "free plan still permissive",
			freePlanLocked: false,
			grandfatherRan: true,
			wantBlockers:   []string{BlockerFreePlanUnlocked},
			wantWarnings:   []string{},
		},
		{
			name:           "admin would lose host capability",
			freePlanLocked: true,
			grandfatherRan: true,
			adminsMissing:  1,
			wantBlockers:   []string{BlockerAdminsWithoutPro},
			wantWarnings:   []string{},
		},
		{
			name:          "nothing prepared at all",
			adminsMissing: 2,
			hostsOnFree:   12,
			liveRooms:     2,
			wantBlockers:  []string{BlockerFreePlanUnlocked, BlockerAdminsWithoutPro},
			wantWarnings:  []string{WarnActiveHostsOnFree, WarnLiveRoomsAtRisk, WarnGrandfatherMissing},
		},
		{
			// The whole point of the feature is cutting off unpaid hosts, so
			// their existence must never veto the switch.
			name:           "unpaid hosts warn but never block",
			freePlanLocked: true,
			grandfatherRan: true,
			hostsOnFree:    30,
			liveRooms:      4,
			wantBlockers:   []string{},
			wantWarnings:   []string{WarnActiveHostsOnFree, WarnLiveRoomsAtRisk},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			blockers, warnings := classifyReadiness(
				tc.freePlanLocked, tc.grandfatherRan, tc.adminsMissing, tc.hostsOnFree, tc.liveRooms)

			if !slices.Equal(blockers, tc.wantBlockers) {
				t.Errorf("blockers = %v, want %v", blockers, tc.wantBlockers)
			}
			if !slices.Equal(warnings, tc.wantWarnings) {
				t.Errorf("warnings = %v, want %v", warnings, tc.wantWarnings)
			}
		})
	}
}
