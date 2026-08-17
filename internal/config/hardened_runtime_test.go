package config

import "testing"

// The bug these cover: StrictRBAC used to be IsProduction(), which needed
// ENVIRONMENT=production. The Railway runbooks never said to set it, so a
// deployed gateway fell back to "development" and ran fail-open — every token
// with an empty permissions array was granted everything.
func TestHardenedRuntime(t *testing.T) {
	cases := []struct {
		name        string
		environment string
		env         map[string]string
		want        bool
	}{
		{"explicit production hardens", "production", nil, true},
		{"explicit prod hardens", "prod", nil, true},

		// The regression itself: deployed, nothing set, must harden.
		{"railway environment with no ENVIRONMENT hardens", "development",
			map[string]string{"RAILWAY_ENVIRONMENT": "production"}, true},
		{"railway project id with no ENVIRONMENT hardens", "development",
			map[string]string{"RAILWAY_PROJECT_ID": "abc-123"}, true},
		{"gin release mode hardens", "development",
			map[string]string{"GIN_MODE": "release"}, true},

		// A laptop stays permissive, which is the whole point of the exemption.
		{"bare local run stays open", "development", nil, false},

		// An explicit dev-like value opts out even on a deployed host, so a
		// staging box can be told to behave like a laptop on purpose.
		{"explicit development on railway opts out", "development",
			map[string]string{"ENVIRONMENT": "development", "RAILWAY_ENVIRONMENT": "production"}, false},
		{"explicit local opts out", "local",
			map[string]string{"ENVIRONMENT": "local", "GIN_MODE": "release"}, false},

		// Anything explicitly set that is not dev-like hardens — staging included.
		{"explicit staging hardens", "staging",
			map[string]string{"ENVIRONMENT": "staging"}, true},
		{"APP_ENV counts as explicit", "staging",
			map[string]string{"APP_ENV": "staging"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Clear every variable the decision reads, so a value leaking in from
			// the developer's shell cannot make this pass or fail by accident.
			for _, k := range []string{"ENVIRONMENT", "APP_ENV", "RAILWAY_ENVIRONMENT", "RAILWAY_PROJECT_ID", "GIN_MODE"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			c := Config{Environment: tc.environment}
			if got := c.HardenedRuntime(); got != tc.want {
				t.Errorf("HardenedRuntime() = %v, want %v", got, tc.want)
			}
			if got := c.StrictRBAC(); got != tc.want {
				t.Errorf("StrictRBAC() = %v, want %v (it must track HardenedRuntime)", got, tc.want)
			}
		})
	}
}
