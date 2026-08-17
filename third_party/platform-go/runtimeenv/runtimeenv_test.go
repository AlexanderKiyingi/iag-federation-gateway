package runtimeenv

import "testing"

// signalVars are every variable the decision reads. Each test clears all of
// them first, so a value leaking in from the developer's shell cannot make a
// case pass or fail by accident.
var signalVars = []string{"ENVIRONMENT", "APP_ENV", "RAILWAY_ENVIRONMENT", "RAILWAY_PROJECT_ID", "GIN_MODE"}

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range signalVars {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// TestHardenedFrom pins the decision table itself, with no environment reading
// involved.
func TestHardenedFrom(t *testing.T) {
	cases := []struct {
		name        string
		environment string
		explicit    bool
		deployed    bool
		want        bool
	}{
		{"production always hardens", "production", false, false, true},
		{"prod always hardens", "prod", false, false, true},
		{"production hardens even when told it is explicit dev-like", "production", true, false, true},
		{"case and padding do not matter", "  PRODUCTION  ", false, false, true},

		// The regression this package exists for: deployed, nothing configured.
		{"deployed with nothing set hardens", "development", false, true, true},
		{"laptop with nothing set stays open", "development", false, false, false},

		// An explicit value is obeyed either way.
		{"explicit development opts out even when deployed", "development", true, true, false},
		{"explicit local opts out even when deployed", "local", true, true, false},
		{"explicit test opts out", "test", true, true, false},
		{"explicit staging hardens", "staging", true, false, true},
		{"explicit unrecognised value hardens", "preprod", true, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HardenedFrom(tc.environment, tc.explicit, tc.deployed); got != tc.want {
				t.Errorf("HardenedFrom(%q, explicit=%v, deployed=%v) = %v, want %v",
					tc.environment, tc.explicit, tc.deployed, got, tc.want)
			}
		})
	}
}

// TestHardenedReadsTheProcess covers the wiring from environment variables to
// the decision, which is where the original bug actually lived.
func TestHardenedReadsTheProcess(t *testing.T) {
	cases := []struct {
		name        string
		environment string
		env         map[string]string
		want        bool
	}{
		{"bare local run stays open", "development", nil, false},

		{"railway environment hardens", "development",
			map[string]string{"RAILWAY_ENVIRONMENT": "production"}, true},
		{"railway project id hardens", "development",
			map[string]string{"RAILWAY_PROJECT_ID": "abc-123"}, true},
		{"gin release mode hardens", "development",
			map[string]string{"GIN_MODE": "release"}, true},
		{"gin release mode is matched case-insensitively", "development",
			map[string]string{"GIN_MODE": "Release"}, true},
		{"gin debug mode does not harden", "development",
			map[string]string{"GIN_MODE": "debug"}, false},

		{"explicit development on railway opts out", "development",
			map[string]string{"ENVIRONMENT": "development", "RAILWAY_ENVIRONMENT": "production"}, false},
		{"APP_ENV counts as explicit", "staging",
			map[string]string{"APP_ENV": "staging"}, true},

		// Whitespace-only is not a configured value; it must not be mistaken for
		// one and flip a deployed host back to permissive.
		{"blank ENVIRONMENT on a deployed host still hardens", "development",
			map[string]string{"ENVIRONMENT": "   ", "GIN_MODE": "release"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			if got := Hardened(tc.environment); got != tc.want {
				t.Errorf("Hardened(%q) with %v = %v, want %v", tc.environment, tc.env, got, tc.want)
			}
		})
	}
}

func TestExplicitlySetAndDeployed(t *testing.T) {
	setEnv(t, nil)
	if ExplicitlySet() {
		t.Error("ExplicitlySet() = true with nothing set")
	}
	if Deployed() {
		t.Error("Deployed() = true on a bare process")
	}

	setEnv(t, map[string]string{"ENVIRONMENT": "staging"})
	if !ExplicitlySet() {
		t.Error("ExplicitlySet() = false with ENVIRONMENT set")
	}

	setEnv(t, map[string]string{"RAILWAY_PROJECT_ID": "x"})
	if !Deployed() {
		t.Error("Deployed() = false with RAILWAY_PROJECT_ID set")
	}
}

func TestClassifiers(t *testing.T) {
	for _, s := range []string{"production", "prod", "PRODUCTION", " prod "} {
		if !IsProduction(s) {
			t.Errorf("IsProduction(%q) = false", s)
		}
	}
	for _, s := range []string{"development", "dev", "local", "test", "DEV"} {
		if !IsDevLike(s) {
			t.Errorf("IsDevLike(%q) = false", s)
		}
	}
	for _, s := range []string{"staging", "preprod", "qa", ""} {
		if IsProduction(s) || IsDevLike(s) {
			t.Errorf("%q should be neither production nor dev-like", s)
		}
	}
}
