package config

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Port                           int    `yaml:"port"`
	RedisAddr                      string `yaml:"redisAddr"`
	IdentityServiceURL             string `yaml:"identityServiceUrl"`
	IdentityServiceApiKey          string `yaml:"identityServiceApiKey"`
	Timezone                       string `yaml:"timezone"`
	LogLevel                       string `yaml:"logLevel"`
	LogFormat                      string `yaml:"logFormat"`
	Env                            string `yaml:"env"`
	DefaultLeaseSeconds            int    `yaml:"defaultLeaseSeconds"`
	RequeueInspectLimit            int    `yaml:"requeueInspectLimit"`
	LocalArtifactsDir              string `yaml:"localArtifactsDir"`
	MaxAttemptsDefault             int    `yaml:"maxAttemptsDefault"`
	BackoffPolicy                  string `yaml:"backoffPolicy"`
	BackoffBaseSeconds             int    `yaml:"backoffBaseSeconds"`
	BackoffMaxSeconds              int    `yaml:"backoffMaxSeconds"`
	WorkerJwksURL                  string `yaml:"workerJwksUrl"`
	WorkerAudience                 string `yaml:"workerAudience"`
	WorkerIssuer                   string `yaml:"workerIssuer"`
	AllowedClockSkewSeconds        int    `yaml:"allowedClockSkewSeconds"`
	WebhookHmacSecret              string `yaml:"webhookHmacSecret"`
	SubscriptionMinIntervalSeconds int    `yaml:"subscriptionMinIntervalSeconds"`
	SubscriptionCleanupIntervalSeconds int `yaml:"subscriptionCleanupIntervalSeconds"`
	ResultWebhookMaxAttempts       int    `yaml:"resultWebhookMaxAttempts"`
	ResultWebhookBaseBackoffSeconds int   `yaml:"resultWebhookBaseBackoffSeconds"`
	ResultWebhookMaxBackoffSeconds  int   `yaml:"resultWebhookMaxBackoffSeconds"`
}

func LoadConfig(filePath string) (*Config, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if v := os.Getenv("PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			c.Port = p
		}
	}
	if v := os.Getenv("REDIS_ADDR"); v != "" {
		c.RedisAddr = v
	}
	if v := os.Getenv("IDENTITY_SERVICE_URL"); v != "" {
		c.IdentityServiceURL = v
	}
	if v := os.Getenv("IDENTITY_SERVICE_API_KEY"); v != "" {
		c.IdentityServiceApiKey = v
	}
	if v := os.Getenv("LOCAL_ARTIFACTS_DIR"); v != "" {
		c.LocalArtifactsDir = v
	}
	if v := os.Getenv("MAX_ATTEMPTS_DEFAULT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.MaxAttemptsDefault = n
		}
	}
	if v := os.Getenv("BACKOFF_POLICY"); v != "" {
		c.BackoffPolicy = v
	}
	if v := os.Getenv("BACKOFF_BASE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.BackoffBaseSeconds = n
		}
	}
	if v := os.Getenv("BACKOFF_MAX_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.BackoffMaxSeconds = n
		}
	}
	if v := os.Getenv("WORKER_JWKS_URL"); v != "" {
		c.WorkerJwksURL = v
	}
	if v := os.Getenv("WORKER_AUDIENCE"); v != "" {
		c.WorkerAudience = v
	}
	if v := os.Getenv("WORKER_ISSUER"); v != "" {
		c.WorkerIssuer = v
	}
	if v := os.Getenv("ALLOWED_CLOCK_SKEW_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.AllowedClockSkewSeconds = n
		}
	}
	if v := os.Getenv("WEBHOOK_HMAC_SECRET"); v != "" {
		c.WebhookHmacSecret = v
	}
	if v := os.Getenv("SUBSCRIPTION_MIN_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.SubscriptionMinIntervalSeconds = n
		}
	}
	if v := os.Getenv("SUBSCRIPTION_CLEANUP_INTERVAL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.SubscriptionCleanupIntervalSeconds = n
		}
	}
	if v := os.Getenv("RESULT_WEBHOOK_MAX_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.ResultWebhookMaxAttempts = n
		}
	}
	if v := os.Getenv("RESULT_WEBHOOK_BASE_BACKOFF_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.ResultWebhookBaseBackoffSeconds = n
		}
	}
	if v := os.Getenv("RESULT_WEBHOOK_MAX_BACKOFF_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.ResultWebhookMaxBackoffSeconds = n
		}
	}

	if c.Port == 0 {
		c.Port = 8080
	}
	if c.RedisAddr == "" {
		c.RedisAddr = "localhost:6379"
	}
	if c.IdentityServiceURL == "" {
		log.Println("Warning: IdentityServiceURL not set, using default")
		c.IdentityServiceURL = "http://api.codecompany.ai"
	}
	if c.IdentityServiceApiKey == "" {
		log.Println("Warning: IdentityServiceApiKey not set (dev only)")
	}
	if c.Timezone == "" {
		c.Timezone = "America/Sao_Paulo"
	}
	if c.LogLevel == "" {
		c.LogLevel = "info"
	}
	if c.LogFormat == "" {
		c.LogFormat = "json"
	}
	if c.Env == "" {
		c.Env = "dev"
	}
	if c.DefaultLeaseSeconds <= 0 {
		c.DefaultLeaseSeconds = 300
	}
	if c.RequeueInspectLimit <= 0 {
		c.RequeueInspectLimit = 200
	}
	if c.LocalArtifactsDir == "" {
		c.LocalArtifactsDir = "/tmp/codeq-artifacts"
	}
	if c.MaxAttemptsDefault <= 0 {
		c.MaxAttemptsDefault = 5
	}
	if c.BackoffBaseSeconds <= 0 {
		c.BackoffBaseSeconds = 5
	}
	if c.BackoffMaxSeconds <= 0 {
		c.BackoffMaxSeconds = 900
	}
	if c.BackoffPolicy == "" {
		c.BackoffPolicy = "exp_full_jitter"
	}
	if c.WorkerAudience == "" {
		c.WorkerAudience = "codeq-worker"
	}
	if c.AllowedClockSkewSeconds <= 0 {
		c.AllowedClockSkewSeconds = 60
	}
	if c.SubscriptionMinIntervalSeconds <= 0 {
		c.SubscriptionMinIntervalSeconds = 5
	}
	if c.SubscriptionCleanupIntervalSeconds <= 0 {
		c.SubscriptionCleanupIntervalSeconds = 60
	}
	if c.ResultWebhookMaxAttempts <= 0 {
		c.ResultWebhookMaxAttempts = 5
	}
	if c.ResultWebhookBaseBackoffSeconds <= 0 {
		c.ResultWebhookBaseBackoffSeconds = 2
	}
	if c.ResultWebhookMaxBackoffSeconds <= 0 {
		c.ResultWebhookMaxBackoffSeconds = 60
	}

	log.Printf("Scheduler Config: {Port:%d Redis:%s Identity:%s TZ:%s Lease:%ds Inspect:%d}\n",
		c.Port, c.RedisAddr, c.IdentityServiceURL, c.Timezone, c.DefaultLeaseSeconds, c.RequeueInspectLimit)
	return &c, nil
}

func (c *Config) Validate() error {
	var errs []string
	env := strings.ToLower(strings.TrimSpace(c.Env))
	dev := env == "dev"

	if c.WorkerJwksURL == "" {
		if !dev {
			errs = append(errs, "workerJwksUrl is required in non-dev")
		}
	} else {
		u, err := url.Parse(c.WorkerJwksURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			errs = append(errs, "workerJwksUrl must be a valid http(s) URL")
		}
	}
	if c.WorkerIssuer == "" && !dev {
		errs = append(errs, "workerIssuer is required in non-dev")
	}
	if c.WorkerAudience == "" {
		errs = append(errs, "workerAudience is required")
	}

	webhooksEnabled := c.SubscriptionMinIntervalSeconds > 0 || c.ResultWebhookMaxAttempts > 0
	if webhooksEnabled && strings.TrimSpace(c.WebhookHmacSecret) == "" && !dev {
		errs = append(errs, "webhookHmacSecret is required when webhooks are enabled")
	}

	if len(errs) > 0 {
		return fmt.Errorf("config validation failed: %s", strings.Join(errs, "; "))
	}
	return nil
}
