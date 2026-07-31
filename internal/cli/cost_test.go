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
		// The per-resource pass takes its service list from the account rollup,
		// so without --costs it has nothing to ask about. Refused rather than
		// ignored: a run that answered the request with silence would look like
		// an account where nothing has a cost.
		{"resources without costs", costFlags{metric: cost.DefaultMetric, maxRequests: 1, resources: true},
			"needs --costs"},
		{"resources with costs", costFlags{metric: cost.DefaultMetric, maxRequests: 1, enabled: true, resources: true}, ""},
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

	// The per-resource overlay bills per service rather than per run, which is
	// a different order of spend from --costs and the only place the user is
	// told so before opting in.
	rf := cmd.Flags().Lookup("cost-resources")
	if rf == nil {
		t.Fatal("--cost-resources flag is missing")
	}
	if rf.DefValue != "false" {
		t.Errorf("--cost-resources defaults to %q, want false", rf.DefValue)
	}
	usage = strings.ToLower(rf.Usage)
	// "per request" as well as "per service": the pass paginates, so the billed
	// unit is the request and a service can cost several. Naming only the service
	// would understate the bill for exactly the estates large enough to paginate.
	for _, want := range []string{"$0.01", "per request", "per service", "14 days"} {
		if !strings.Contains(usage, want) {
			t.Errorf("--cost-resources help does not mention %q: %q", want, rf.Usage)
		}
	}
	// The console preference is not retroactive, so a user who enables it after
	// a run cannot recover that run's data by re-running against the same days.
	if !strings.Contains(usage, "retroactive") {
		t.Errorf("--cost-resources help does not warn that the console opt-in is not retroactive: %q", rf.Usage)
	}

	// Rejected at the flag, before credentials are loaded.
	optIn := scanCmd()
	optIn.SetOut(&bytes.Buffer{})
	optIn.SetErr(&bytes.Buffer{})
	optIn.SetArgs([]string{"--cost-resources"})
	if err := optIn.Execute(); err == nil || !strings.Contains(err.Error(), "needs --costs") {
		t.Errorf("scan with --cost-resources alone returned %v, want a validation error", err)
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
