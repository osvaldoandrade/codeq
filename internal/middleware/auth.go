package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/osvaldoandrade/codeq/pkg/config"

	"github.com/gin-gonic/gin"
)

type lookupReq struct {
	IdToken string `json:"idToken"`
}
type lookupUser struct {
	LocalId string `json:"localId"`
	Email   string `json:"email"`
	Role    string `json:"role,omitempty"`
}
type lookupResp struct {
	Users []lookupUser `json:"users"`
}

func AuthMiddleware(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, err := validateProducerToken(c.Request.Context(), cfg, c.GetHeader("Authorization"))
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": err.Error()})
			return
		}
		c.Set("userEmail", user.Email)
		role := strings.ToUpper(strings.TrimSpace(user.Role))
		if role == "" && cfg.Env == "dev" {
			role = strings.ToUpper(strings.TrimSpace(c.GetHeader("X-Role")))
		}
		if role == "" {
			role = "USER"
		}
		c.Set("userRole", role)
		c.Next()
	}
}

func validateProducerToken(ctx context.Context, cfg *config.Config, authHeader string) (*lookupUser, error) {
	if authHeader == "" {
		return nil, fmt.Errorf("missing Authorization header")
	}
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || parts[0] != "Bearer" {
		return nil, fmt.Errorf("invalid Authorization format")
	}
	idToken := parts[1]

	url := fmt.Sprintf("%s/v1/accounts/lookup?key=%s", cfg.IdentityServiceURL, cfg.IdentityServiceApiKey)
	body, _ := json.Marshal(lookupReq{IdToken: idToken})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewBuffer(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("identity lookup error")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		log.Printf("[Auth] identity non-200: %d %s", resp.StatusCode, string(b))
		return nil, fmt.Errorf("invalid token")
	}
	var lr lookupResp
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil || len(lr.Users) == 0 {
		return nil, fmt.Errorf("user not found")
	}
	user := lr.Users[0]
	return &user, nil
}
