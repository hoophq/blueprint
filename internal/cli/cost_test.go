package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/hoophq/blueprint/internal/cost"
)

// Cost flags are validated before the scan starts, so a typo surfaces in a
// second rather than after a multi-minute scan — and long before it could
// configure a billed API call.
func TestCostFlagsValidate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		flags   costFlags
		wantErr string
	}{
		{"defaults", costFlags{metric: cost.DefaultMetric, maxRequests: cost.DefaultMaxRequests}, ""},
		{"every advertised metric", costFlags{metric: "net_unblended", maxRequests: 1}, ""},
		{"typo", costFlags{metric: "amortised", maxRequests: 1}, "not valid"},
		// Deliberately absent rather than overlooked: a blended rate is an
		// intra-organization average, so a per-account blended figure
		// reconciles to no invoice.
		{"blended is not offered", costFlags{metric: "blended", maxRequests: 1}, "not valid"},
		{"AWS name is not the flag value", costFlags{metric: "AmortizedCost", maxRequests: 1}, "not valid"},
		{"empty metric", costFlags{metric: "", maxRequests: 1}, "not valid"},
		// A zero budget would silently collect nothing; that is a mistake
		// worth reporting, not a quiet no-op.
		{"zero budget", costFlags{metric: cost.DefaultMetric, maxRequests: 0}, "at least 1"},
		{"negative budget", costFlags{metric: cost.DefaultMetric, maxRequests: -1}, "at least 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.flags.validate()
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("validate() = %v, want nil", err)
			case tc.wantErr != "" && err == nil:
				t.Errorf("validate() = nil, want an error mentioning %q", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Errorf("validate() = %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}

	// A rejected metric must name the alternatives; the user cannot guess
	// that the flag takes "amortized" and not "AmortizedCost".
	err := costFlags{metric: "nope", maxRequests: 1}.validate()
	for _, m := range cost.Metrics() {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("error does not list the valid metric %q: %v", m, err)
		}
	}
}

// Cost is off unless asked for, and asking for it prints the price before
// spending it. A read-only tool must not spend money by default or quietly.
func TestCostFlagDefaultsAndHelp(t *testing.T) {
	cmd := scanCmd()

	f := cmd.Flags().Lookup("costs")
	if f == nil {
		t.Fatal("--costs flag is missing")
	}
	if f.DefValue != "false" {
		t.Errorf("--costs defaults to %q, want false", f.DefValue)
	}
	// The help text is the only warning a user gets before opting in.
	usage := strings.ToLower(f.Usage)
	if !strings.Contains(usage, "$0.01") {
		t.Errorf("--costs help does not state the per-request price: %q", f.Usage)
	}

	if got := cmd.Flags().Lookup("cost-metric").DefValue; got != cost.DefaultMetric {
		t.Errorf("--cost-metric defaults to %q, want %q", got, cost.DefaultMetric)
	}
	if got := cmd.Flags().Lookup("cost-max-requests").DefValue; got == "0" || got == "" {
		t.Errorf("--cost-max-requests defaults to %q, want a real budget", got)
	}

	// A bad metric is rejected without touching AWS: no credentials are
	// loaded, so this would fail differently if validation ran later.
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--cost-metric", "bogus"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "not valid") {
		t.Errorf("scan with a bad --cost-metric returned %v, want a validation error", err)
	}
}
