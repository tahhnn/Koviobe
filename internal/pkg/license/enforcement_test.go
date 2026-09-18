package license

import "testing"

// The cache is the reason these tests can exist at all: while Enforcing() read
// config.AppConfig, no test could reach the enforcing branch without a loaded
// config, so every license test ran against the OFF path only.
func TestEnforcingDefaultsOff(t *testing.T) {
	enforcement.Store(false)
	envPinned.Store(false)
	if Enforcing() {
		t.Fatal("zero value must be off — a process that has not resolved the flag cannot answer 'on'")
	}
}

func TestEnforcingReflectsCache(t *testing.T) {
	t.Cleanup(func() { enforcement.Store(false) })

	enforcement.Store(true)
	if !Enforcing() {
		t.Fatal("Enforcing() must read the cache")
	}
	enforcement.Store(false)
	if Enforcing() {
		t.Fatal("Enforcing() must follow the cache back down")
	}
}

func TestSetEnforcementRefusesWhenPinned(t *testing.T) {
	t.Cleanup(func() {
		envPinned.Store(false)
		enforcement.Store(false)
	})

	envPinned.Store(true)
	enforcement.Store(true)

	if err := SetEnforcement(false, 1); err != ErrEnforcementPinned {
		t.Fatalf("pinned SetEnforcement = %v, want ErrEnforcementPinned", err)
	}
	if !Enforcing() {
		t.Fatal("a refused write must leave the cache untouched")
	}
	if !EnvPinned() {
		t.Fatal("EnvPinned must report the pin")
	}
}

func TestRefreshIsNoopWhenPinned(t *testing.T) {
	t.Cleanup(func() {
		envPinned.Store(false)
		enforcement.Store(false)
	})

	envPinned.Store(true)
	enforcement.Store(true)
	// No DB in unit tests: if RefreshEnforcement did not short-circuit on the
	// pin, the failed read path would run and must still not flip the flag.
	RefreshEnforcement()
	if !Enforcing() {
		t.Fatal("refresh must not un-arm a pinned process")
	}
}

func TestRefreshKeepsLastGoodValueOnFailedRead(t *testing.T) {
	t.Cleanup(func() { enforcement.Store(false) })

	enforcement.Store(true)
	// db.DB is nil here, so the read fails. A transient failure must not
	// silently disarm gates somebody deliberately armed.
	RefreshEnforcement()
	if !Enforcing() {
		t.Fatal("a failed refresh must keep the last good value, not fall back to false")
	}
}
