package orgmode

import (
	"testing"

	orgtypes "github.com/aws/aws-sdk-go-v2/service/organizations/types"
)

func TestRoleARN(t *testing.T) {
	tests := []struct {
		name      string
		partition string
		accountID string
		roleName  string
		want      string
	}{
		{
			name:      "default org access role",
			partition: "aws",
			accountID: "123456789012",
			roleName:  "OrganizationAccountAccessRole",
			want:      "arn:aws:iam::123456789012:role/OrganizationAccountAccessRole",
		},
		{
			name:      "custom read-only role",
			partition: "aws",
			accountID: "000000000000",
			roleName:  "dbcensus-readonly",
			want:      "arn:aws:iam::000000000000:role/dbcensus-readonly",
		},
		{
			name:      "role with path-like name",
			partition: "aws",
			accountID: "999999999999",
			roleName:  "audit/dbcensus",
			want:      "arn:aws:iam::999999999999:role/audit/dbcensus",
		},
		{
			name:      "govcloud partition",
			partition: "aws-us-gov",
			accountID: "123456789012",
			roleName:  "OrganizationAccountAccessRole",
			want:      "arn:aws-us-gov:iam::123456789012:role/OrganizationAccountAccessRole",
		},
		{
			name:      "china partition",
			partition: "aws-cn",
			accountID: "123456789012",
			roleName:  "OrganizationAccountAccessRole",
			want:      "arn:aws-cn:iam::123456789012:role/OrganizationAccountAccessRole",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := roleARN(tc.partition, tc.accountID, tc.roleName); got != tc.want {
				t.Errorf("roleARN(%q, %q, %q) = %q, want %q", tc.partition, tc.accountID, tc.roleName, got, tc.want)
			}
		})
	}
}

func TestPartitionFromARN(t *testing.T) {
	tests := []struct {
		name string
		arn  string
		want string
	}{
		{
			name: "commercial org account ARN",
			arn:  "arn:aws:organizations::123456789012:account/o-exampleorgid/210987654321",
			want: "aws",
		},
		{
			name: "govcloud org account ARN",
			arn:  "arn:aws-us-gov:organizations::123456789012:account/o-exampleorgid/210987654321",
			want: "aws-us-gov",
		},
		{
			name: "china org account ARN",
			arn:  "arn:aws-cn:organizations::123456789012:account/o-exampleorgid/210987654321",
			want: "aws-cn",
		},
		{
			name: "empty ARN falls back to aws",
			arn:  "",
			want: "aws",
		},
		{
			name: "malformed ARN falls back to aws",
			arn:  "not-an-arn",
			want: "aws",
		},
		{
			name: "empty partition segment falls back to aws",
			arn:  "arn::organizations::123456789012:account/o-x/210987654321",
			want: "aws",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := partitionFromARN(tc.arn); got != tc.want {
				t.Errorf("partitionFromARN(%q) = %q, want %q", tc.arn, got, tc.want)
			}
		})
	}
}

func TestIsActive(t *testing.T) {
	tests := []struct {
		name string
		acct orgtypes.Account
		want bool
	}{
		{
			name: "state active",
			acct: orgtypes.Account{State: orgtypes.AccountStateActive},
			want: true,
		},
		{
			name: "state suspended overrides active status",
			acct: orgtypes.Account{State: orgtypes.AccountStateSuspended, Status: orgtypes.AccountStatusActive},
			want: false,
		},
		{
			name: "no state, status active (legacy responses)",
			acct: orgtypes.Account{Status: orgtypes.AccountStatusActive},
			want: true,
		},
		{
			name: "no state, status suspended",
			acct: orgtypes.Account{Status: orgtypes.AccountStatusSuspended},
			want: false,
		},
		{
			name: "empty account",
			acct: orgtypes.Account{},
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isActive(tc.acct); got != tc.want {
				t.Errorf("isActive(%+v) = %v, want %v", tc.acct, got, tc.want)
			}
		})
	}
}
