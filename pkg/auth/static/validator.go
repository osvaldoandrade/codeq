package static

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/osvaldoandrade/codeq/pkg/auth"
)

type validatorConfig struct {
	// Token is the exact bearer token value expected by this validator.
	Token string `json:"token"`

	// Subject is returned as claims.Subject.
	Subject string `json:"subject,omitempty"`

	// Email is returned as claims.Email (producer auth).
	Email string `json:"email,omitempty"`

	// Scopes is returned as claims.Scopes (worker auth scope checks).
	Scopes []string `json:"scopes,omitempty"`

	// EventTypes is returned as claims.EventTypes (worker claim authorization).
	EventTypes []string `json:"eventTypes,omitempty"`

	// Raw is returned as claims.Raw (used for role-based checks, etc).
	Raw map[string]any `json:"raw,omitempty"`
}

type validator struct {
	cfg validatorConfig
}

func NewValidatorFromJSON(raw json.RawMessage) (auth.Validator, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return nil, errors.New("static auth: missing config")
	}

	var cfg validatorConfig
	// Allow config to be either:
	// - JSON object: {"token":"...","subject":"..."}
	// - JSON string: "token-value"
	if raw[0] == '"' {
		if err := json.Unmarshal(raw, &cfg.Token); err != nil {
			return nil, fmtError("static auth: invalid config", err)
		}
	} else {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmtError("static auth: invalid config", err)
		}
	}

	cfg.Token = strings.TrimSpace(cfg.Token)
	if cfg.Token == "" {
		return nil, errors.New("static auth: token is required")
	}
	cfg.Subject = strings.TrimSpace(cfg.Subject)
	if cfg.Subject == "" {
		cfg.Subject = "static"
	}
	if cfg.Raw == nil {
		cfg.Raw = map[string]any{}
	}

	return &validator{cfg: cfg}, nil
}

func (v *validator) Validate(token string) (*auth.Claims, error) {
	if strings.TrimSpace(token) != v.cfg.Token {
		return nil, errors.New("invalid token")
	}
	return &auth.Claims{
		Subject:    v.cfg.Subject,
		Email:      v.cfg.Email,
		Scopes:     v.cfg.Scopes,
		EventTypes: v.cfg.EventTypes,
		Raw:        v.cfg.Raw,
	}, nil
}

func init() {
	auth.RegisterProvider("static", NewValidatorFromJSON)
}

func fmtError(msg string, err error) error {
	if err == nil {
		return errors.New(msg)
	}
	return errors.New(msg + ": " + err.Error())
}
