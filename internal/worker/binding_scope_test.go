package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/osvaldoandrade/codeq/internal/authclaims"
	"github.com/osvaldoandrade/codeq/pkg/auth"
)

type mapValidator map[string]*auth.Claims

func (m mapValidator) Validate(token string) (*auth.Claims, error) {
	if claims, ok := m[token]; ok {
		return claims, nil
	}
	return nil, errors.New("invalid token")
}

func TestAuthenticateRefusesBindingScopedTokens(t *testing.T) {
	binding := &auth.Claims{
		Subject: "codefoundry:workload:a:b:c:d", EventTypes: []string{"topic-a"},
		Scopes: []string{"codeq:claim"}, Raw: map[string]interface{}{authclaims.BindingClaim: map[string]interface{}{}},
	}
	publish := &auth.Claims{Subject: "p", Scopes: []string{authclaims.PublishScope}, Raw: map[string]interface{}{"tid": "conveste"}}
	legacy := &auth.Claims{Subject: "legacy-producer", Raw: map[string]interface{}{"tenantId": "local-tenant"}}
	s := &Server{
		Validator:             mapValidator{"binding": binding},
		ProducerValidator:     mapValidator{"publish": publish, "legacy": legacy},
		AllowProducerAsWorker: true,
		WorkerAudience:        "codeq-worker",
	}
	for _, token := range []string{"binding", "publish"} {
		if _, err := s.authenticate(context.Background(), token); !errors.Is(err, authclaims.ErrStreamNotAllowed) {
			t.Fatalf("%s: err = %v, want ErrStreamNotAllowed", token, err)
		}
	}
	claims, err := s.authenticate(context.Background(), "legacy")
	if err != nil || len(claims.EventTypes) != 1 || claims.EventTypes[0] != "*" {
		t.Fatalf("legacy promotion changed: %v %+v", err, claims)
	}
}
