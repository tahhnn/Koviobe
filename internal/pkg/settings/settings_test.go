package settings

import "testing"

func TestParseBool(t *testing.T) {
	tests := []struct {
		raw     string
		def     bool
		want    bool
		wantErr bool
	}{
		{"true", false, true, false},
		{"false", true, false, false},
		{"TRUE", false, true, false},
		{"1", false, true, false},
		{"0", true, false, false},
		{" true ", false, true, false},
		// A row that was never written cannot reach here (GetBool short-circuits
		// on ErrNotFound), so an empty value means a corrupted row: report the
		// error and hand back the default rather than guessing.
		{"", false, false, true},
		{"", true, true, true},
		{"rác", true, true, true},
	}

	for _, tc := range tests {
		got, err := parseBool(tc.raw, tc.def)
		if (err != nil) != tc.wantErr {
			t.Errorf("parseBool(%q) err = %v, wantErr %v", tc.raw, err, tc.wantErr)
		}
		if got != tc.want {
			t.Errorf("parseBool(%q, %t) = %t, want %t", tc.raw, tc.def, got, tc.want)
		}
	}
}
