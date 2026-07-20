package authclaims

import (
	"errors"
	"math/rand"
	"strings"
	"testing"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

const (
	testPayments              = "payments"
	testIdentity              = "identity"
	testAnalytics             = "analytics"
	testPlatform              = "platform"
	testClaimTenantID         = "tenantId"
	testClaimOrganizationID   = "organizationId"
	testClaimOrganizationPath = "organization_id"
)

func TestResolveTenantIDCompatibilityMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		claims  *auth.Claims
		want    string
		wantErr error
	}{
		{name: "nil claims", wantErr: ErrTenantMissing},
		{name: "canonical tid", claims: claims(map[string]any{claimTID: " payments "}, "ignored"), want: testPayments},
		{name: "firebase camel case", claims: claims(map[string]any{testClaimTenantID: testIdentity}, ""), want: testIdentity},
		{name: "snake case tenant", claims: claims(map[string]any{claimTenantIDSnake: testAnalytics}, ""), want: testAnalytics},
		{name: "organization camel case", claims: claims(map[string]any{testClaimOrganizationID: testPlatform}, ""), want: testPlatform},
		{name: "organization snake case", claims: claims(map[string]any{testClaimOrganizationPath: testPlatform}, ""), want: testPlatform},
		{name: "matching aliases", claims: claims(map[string]any{claimTID: testPayments, testClaimTenantID: testPayments, testClaimOrganizationPath: testPayments}, "other"), want: testPayments},
		{name: "subject fallback", claims: claims(map[string]any{}, "single-tenant"), want: "single-tenant"},
		{name: "canonical conflict", claims: claims(map[string]any{claimTID: testPayments, testClaimTenantID: testAnalytics}, testPayments), wantErr: ErrTenantConflict},
		{name: "legacy conflict", claims: claims(map[string]any{claimTenantIDSnake: testPayments, testClaimOrganizationID: testAnalytics}, testPayments), wantErr: ErrTenantConflict},
		{name: "blank claim does not fall back", claims: claims(map[string]any{claimTID: " "}, testPayments), wantErr: ErrTenantMalformed},
		{name: "non-string claim", claims: claims(map[string]any{claimTID: 7}, testPayments), wantErr: ErrTenantMalformed},
		{name: "unsafe tenant", claims: claims(map[string]any{claimTID: "../payments"}, testPayments), wantErr: ErrTenantMalformed},
		{name: "missing subject and claims", claims: claims(nil, ""), wantErr: ErrTenantMissing},
		{name: "unsafe subject fallback", claims: claims(nil, "user@example.com"), wantErr: ErrTenantMalformed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := ResolveTenantID(test.claims)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("ResolveTenantID() error = %v, want %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("ResolveTenantID() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestResolveTenantIDAliasOrderDoesNotChangeResult(t *testing.T) {
	t.Parallel()
	aliases := []string{claimTID, testClaimTenantID, claimTenantIDSnake, testClaimOrganizationID, testClaimOrganizationPath}
	for seed := int64(0); seed < 100; seed++ {
		rand.New(rand.NewSource(seed)).Shuffle(len(aliases), func(left, right int) {
			aliases[left], aliases[right] = aliases[right], aliases[left]
		})
		raw := make(map[string]any, len(aliases))
		for _, alias := range aliases {
			raw[alias] = testPayments
		}
		got, err := ResolveTenantID(claims(raw, "fallback"))
		if err != nil || got != testPayments {
			t.Fatalf("seed %d: got tenant=%q err=%v", seed, got, err)
		}
	}
}

func FuzzResolveTenantID(f *testing.F) {
	f.Add("payments", "payments", "payments")
	f.Add("payments", "analytics", "fallback")
	f.Add("", "", "single-tenant")
	f.Fuzz(func(t *testing.T, canonical, legacy, subject string) {
		raw := map[string]any{}
		if canonical != "" {
			raw[claimTID] = canonical
		}
		if legacy != "" {
			raw["tenantId"] = legacy
		}
		resolved, err := ResolveTenantID(claims(raw, subject))
		if err == nil && !tenantPattern.MatchString(resolved) {
			t.Fatalf("accepted unsafe tenant %q", resolved)
		}
		if err == nil && canonical != "" && legacy != "" && strings.TrimSpace(canonical) != strings.TrimSpace(legacy) {
			t.Fatalf("accepted conflicting claims canonical=%q legacy=%q", canonical, legacy)
		}
	})
}

func claims(raw map[string]any, subject string) *auth.Claims {
	return &auth.Claims{Raw: raw, Subject: subject}
}
