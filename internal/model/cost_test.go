package model

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// The cost report reaches Sort with its slices in whatever order the Cost
// Explorer response and the collector's own maps produced, so two runs over an
// identical bill would otherwise write two different JSON files. Every slice
// the report owns must land in the same order regardless of how it was built.
func TestCostReportSortIsOrderIndependent(t *testing.T) {
	// build assembles the same report twice, differing only in the order the
	// entries were appended in — the axis a map-ranging collector varies on.
	build := func(reverse bool) *CostReport {
		services := []NamedAmount{
			{Name: "Amazon DynamoDB", Amount: "10.00"},
			{Name: "Amazon Redshift", Amount: "20.00"},
			// Two records sharing a name: the amount tie-break is what keeps
			// these two from swapping places under an unstable sort.
			{Name: "Amazon Redshift", Amount: "5.00"},
		}
		unattributed := []NamedAmount{
			{Name: "Tax", Amount: "3.00"},
			{Name: "Credit", Amount: "-1.00"},
		}
		currencies := []CostByCurrency{
			{Currency: "USD", Total: "37.00", Attributed: "35.00", Unattributed: "2.00",
				Services: services, UnattributedRecords: unattributed},
			{Currency: "EUR", Total: "1.00", Attributed: "1.00", Unattributed: "0.00"},
		}
		accounts := []string{"222222222222", "111111111111"}
		if reverse {
			slices.Reverse(services)
			slices.Reverse(unattributed)
			slices.Reverse(currencies)
			slices.Reverse(accounts)
		}
		return &CostReport{
			Window:     CostWindow{Start: "2026-06-01", End: "2026-07-01", Label: "2026-06"},
			Metric:     "AmortizedCost",
			Accounts:   accounts,
			Currencies: currencies,
		}
	}

	forward, reverse := build(false), build(true)
	forward.Sort()
	reverse.Sort()

	a, err := json.Marshal(forward)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	b, err := json.Marshal(reverse)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("append order changed the JSON:\n %s\n %s", a, b)
	}

	// Spell the invariants out too, so a failure says which slice drifted
	// rather than only that two blobs differ.
	if !sort.SliceIsSorted(forward.Currencies, func(i, j int) bool {
		return forward.Currencies[i].Currency < forward.Currencies[j].Currency
	}) {
		t.Errorf("Currencies not sorted: %v", forward.Currencies)
	}
	if !sort.StringsAreSorted(forward.Accounts) {
		t.Errorf("Accounts not sorted: %v", forward.Accounts)
	}
	for _, cur := range forward.Currencies {
		for name, list := range map[string][]NamedAmount{
			"Services": cur.Services, "UnattributedRecords": cur.UnattributedRecords,
		} {
			if !sort.SliceIsSorted(list, func(i, j int) bool {
				if list[i].Name != list[j].Name {
					return list[i].Name < list[j].Name
				}
				return list[i].Amount < list[j].Amount
			}) {
				t.Errorf("%s.%s not sorted: %v", cur.Currency, name, list)
			}
		}
	}
}

// Cost is absent from the JSON when nobody priced the resource — not present
// and null. A reader deserializing into a typed struct sees the same thing
// either way, but a reader eyeballing the artifact or querying it with jq does
// not: a null invites "the cost is nothing", which is the one reading the
// pointer exists to prevent.
func TestResourceCostAbsentRatherThanNull(t *testing.T) {
	data, err := json.Marshal(Resource{ARN: "arn:aws:rds:us-east-1:1:db:x"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, key := range []string{"cost", "cost_unavailable"} {
		if strings.Contains(string(data), `"`+key+`"`) {
			t.Errorf("unpriced resource carries a %q key: %s", key, data)
		}
	}
}

// The two absences are different statements and must stay distinguishable in
// the artifact: a blank cost with a reason beside it says a source looked and
// found nothing, while a blank cost with no reason says nothing looked.
func TestResourceCostUnavailableIsWrittenWhenNamed(t *testing.T) {
	r := Resource{ARN: "arn:aws:dynamodb:us-east-1:1:table/x"}
	r.CostUnavailable = "no Cost Optimization Hub recommendation for this resource"
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(data), `"cost_unavailable":"no Cost Optimization Hub`) {
		t.Errorf("named absence missing from JSON: %s", data)
	}
	if strings.Contains(string(data), `"cost"`) {
		t.Errorf("unpriced resource carries a cost object: %s", data)
	}
}

// A reported zero is a finding, not an absence, and has to survive to the
// artifact intact — including the currency and the estimated flag, which say
// which zero it is.
func TestResourceCostZeroSurvivesMarshalling(t *testing.T) {
	r := Resource{ARN: "arn:aws:rds:us-east-1:1:db:x"}
	r.Cost = &ResourceCost{Amount: "0.00", Currency: "USD", Method: CostMethodCOH}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, want := range []string{`"amount":"0.00"`, `"currency":"USD"`, `"estimated":false`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("missing %s in: %s", want, data)
		}
	}
}

// Caveats are deliberately not sorted: they are disclosures written in the
// order the source's own conditions were evaluated, and reordering them would
// change what the list reads like without changing what it says. Determinism
// here comes from that fixed construction order, so what is worth pinning is
// that nothing downstream reorders them and that the same resource marshals
// the same way twice.
func TestResourceCostCaveatsKeepSourceOrder(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 14)
	caveats := []string{"covers storage only", "resource is younger than the window"}

	var first string
	for range 2 {
		r := Resource{ARN: "arn:aws:rds:us-east-1:1:db:x"}
		r.Cost = &ResourceCost{
			Amount: "1.00", Currency: "USD", Method: CostMethodCOH, Estimated: true,
			ObservedFrom: &from, ObservedTo: &to, Caveats: caveats,
		}
		data, err := json.Marshal(r)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if first == "" {
			first = string(data)
		} else if string(data) != first {
			t.Errorf("two marshals of one resource differ:\n %s\n %s", first, data)
		}
	}
	if !strings.Contains(first, `["covers storage only","resource is younger than the window"]`) {
		t.Errorf("caveats reordered or reshaped: %s", first)
	}
}
