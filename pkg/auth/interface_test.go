package auth

import (
	"context"
	"testing"
)

type plainValidator struct{}

func (plainValidator) Validate(string) (*Claims, error) { return &Claims{Subject: "plain"}, nil }

type ctxKey struct{}

type contextValidator struct{ plainValidator }

func (contextValidator) ValidateContext(ctx context.Context, _ string) (*Claims, error) {
	subject, _ := ctx.Value(ctxKey{}).(string)
	return &Claims{Subject: subject}, nil
}

func TestValidateContextUsesContextValidatorWhenAvailable(t *testing.T) {
	ctx := context.WithValue(context.Background(), ctxKey{}, "from-context")
	if claims, err := ValidateContext(ctx, contextValidator{}, "t"); err != nil || claims.Subject != "from-context" {
		t.Fatalf("context validator not used: %+v %v", claims, err)
	}
	if claims, err := ValidateContext(ctx, plainValidator{}, "t"); err != nil || claims.Subject != "plain" {
		t.Fatalf("plain validator not used: %+v %v", claims, err)
	}
}
