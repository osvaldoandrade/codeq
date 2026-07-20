package middleware

import (
	"errors"
	"testing"

	"github.com/osvaldoandrade/codeq/internal/authclaims"
	"github.com/osvaldoandrade/codeq/pkg/auth"
)

func TestExtractTenantID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		claims  *auth.Claims
		want    string
		wantErr error
	}{
		{name: "nil claims", claims: nil, wantErr: authclaims.ErrTenantMissing},
		{name: "canonical tid", claims: &auth.Claims{Subject: "user-1", Raw: map[string]interface{}{"tid": " payments "}}, want: "payments"},
		{name: "matching aliases", claims: &auth.Claims{Subject: "user-1", Raw: map[string]interface{}{"tid": "payments", "tenantId": "payments"}}, want: "payments"},
		{name: "conflicting alias", claims: &auth.Claims{Subject: "user-1", Raw: map[string]interface{}{"tid": "payments", "tenantId": "identity"}}, wantErr: authclaims.ErrTenantConflict},
		{name: "firebase tenantId", claims: &auth.Claims{Raw: map[string]interface{}{"tenantId": "identity"}}, want: "identity"},
		{name: "snake case tenant", claims: &auth.Claims{Raw: map[string]interface{}{"tenant_id": "analytics"}}, want: "analytics"},
		{name: "organization", claims: &auth.Claims{Raw: map[string]interface{}{"organizationId": "platform"}}, want: "platform"},
		{name: "subject fallback", claims: &auth.Claims{Subject: "single-tenant", Raw: map[string]interface{}{}}, want: "single-tenant"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := extractTenantID(test.claims)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("extractTenantID() error = %v, want %v", err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("extractTenantID() = %q, want %q", got, test.want)
			}
		})
	}
}
