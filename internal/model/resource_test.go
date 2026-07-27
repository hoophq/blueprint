package model

import (
	"sort"
	"testing"
)

// Sort's key must be unique even when names collide: resource types whose
// Name comes from an optional tag (EBS volumes, NAT gateways, …) can share an
// empty name within one (account, region, service), the runner appends in
// goroutine-completion order, and sort.Slice is unstable — only the ARN
// tie-break keeps the JSON artifact byte-for-byte deterministic.
func TestSortTieBreaksOnARN(t *testing.T) {
	mk := func(arn string) Resource {
		return Resource{ARN: arn, AccountID: "1", Region: "us-east-1", Service: "ec2", Name: ""}
	}
	arns := []string{
		"arn:aws:ec2:us-east-1:1:volume/vol-ccc",
		"arn:aws:ec2:us-east-1:1:volume/vol-aaa",
		"arn:aws:ec2:us-east-1:1:volume/vol-bbb",
	}
	// Both insertion orders must converge on the same output order.
	for _, order := range [][]int{{0, 1, 2}, {2, 0, 1}} {
		s := &Snapshot{}
		for _, i := range order {
			s.Resources = append(s.Resources, mk(arns[i]))
		}
		s.Sort()
		for i := 1; i < len(s.Resources); i++ {
			if s.Resources[i-1].ARN >= s.Resources[i].ARN {
				t.Fatalf("insertion order %v: resources not ARN-ordered: %q before %q",
					order, s.Resources[i-1].ARN, s.Resources[i].ARN)
			}
		}
	}
}

func TestFinalizeSortsScopeLists(t *testing.T) {
	s := &Snapshot{
		Accounts: []string{"222222222222", "111111111111"},
		Regions:  []string{"us-west-2", "sa-east-1"},
		Services: []string{"redshift", "dynamodb", "rds"},
	}
	s.Finalize()
	for name, list := range map[string][]string{
		"Accounts": s.Accounts, "Regions": s.Regions, "Services": s.Services,
	} {
		if !sort.StringsAreSorted(list) {
			t.Errorf("%s not sorted after Finalize: %v", name, list)
		}
	}
}
