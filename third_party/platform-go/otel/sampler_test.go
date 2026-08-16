package otel

import (
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestSamplerFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		sampler string
		arg     string
		want    string
	}{
		{"unset defaults to always-on", "", "", sdktrace.AlwaysSample().Description()},
		{"ratio alone implies parent-based", "", "0.1", sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1)).Description()},
		{"always_off", "always_off", "", sdktrace.NeverSample().Description()},
		{"parentbased_always_on", "parentbased_always_on", "", sdktrace.ParentBased(sdktrace.AlwaysSample()).Description()},
		{"traceidratio", "traceidratio", "0.25", sdktrace.TraceIDRatioBased(0.25).Description()},
		{"case and spacing tolerated", "  ParentBased_TraceIDRatio  ", "0.5", sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.5)).Description()},
		{"zero ratio is a valid choice", "traceidratio", "0", sdktrace.TraceIDRatioBased(0).Description()},

		// A bad ratio must not blind the platform — degrade to more traces, not none.
		{"unparseable ratio", "parentbased_traceidratio", "ten percent", sdktrace.AlwaysSample().Description()},
		{"out of range ratio", "traceidratio", "1.5", sdktrace.AlwaysSample().Description()},
		{"negative ratio", "traceidratio", "-0.2", sdktrace.AlwaysSample().Description()},
		{"unknown sampler name", "aggressive", "0.1", sdktrace.AlwaysSample().Description()},
		{"ratio sampler with no arg", "traceidratio", "", sdktrace.AlwaysSample().Description()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OTEL_TRACES_SAMPLER", tc.sampler)
			t.Setenv("OTEL_TRACES_SAMPLER_ARG", tc.arg)
			if got := SamplerFromEnv().Description(); got != tc.want {
				t.Errorf("SamplerFromEnv() = %q, want %q", got, tc.want)
			}
		})
	}
}
