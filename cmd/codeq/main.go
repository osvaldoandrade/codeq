package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/briandowns/spinner"
	"github.com/fatih/color"
	"github.com/schollz/progressbar/v3"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

type client struct {
	baseURL       string
	producerToken string
	workerToken   string
	admin         bool
	httpClient    *http.Client
}

type taskResp struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Payload string `json:"payload"`
	Status  string `json:"status"`
}

type queueStats struct {
	Command    string `json:"command"`
	Ready      int64  `json:"ready"`
	Delayed    int64  `json:"delayed"`
	InProgress int64  `json:"inProgress"`
	DLQ        int64  `json:"dlq"`
}

type ui struct {
	title func(a ...any) string
	ok    func(a ...any) string
	info  func(a ...any) string
	warn  func(a ...any) string
	err   func(a ...any) string
	dim   func(a ...any) string
}

const defaultIAMBaseURL = "https://api.storifly.ai/v1/accounts"

type profile struct {
	BaseURL       string     `yaml:"baseUrl"`
	IAMBaseURL    string     `yaml:"iamBaseUrl"`
	IAMAPIKey     string     `yaml:"iamApiKey"`
	Token         string     `yaml:"token"`
	ProducerToken string     `yaml:"producerToken"`
	WorkerToken   string     `yaml:"workerToken"`
	Auth          authConfig `yaml:"auth"`
	Admin         bool       `yaml:"admin"`
}

type cliConfig struct {
	CurrentProfile string             `yaml:"currentProfile"`
	Profiles       map[string]profile `yaml:"profiles"`
}

type authConfig struct {
	Login    loginConfig    `yaml:"login"`
	Exchange exchangeConfig `yaml:"exchange"`
}

type installOptions struct {
	Target             string
	Size               string
	Release            string
	Namespace          string
	Chart              string
	OutputDir          string
	Image              string
	Domain             string
	RedisAddr          string
	IdentityServiceURL string
	WorkerJwksURL      string
	WorkerIssuer       string
	WorkerAudience     string
	WebhookHmacSecret  string
	Build              bool
	BuildContext       string
	Execute            bool
	NoPrompt           bool
}

type installProfile struct {
	Name                 string
	Description          string
	Replicas             int
	MinReplicas          int
	MaxReplicas          int
	TargetCPU            int
	CPURequest           string
	MemoryRequest        string
	CPULimit             string
	MemoryLimit          string
	KVRocksSize          string
	KVRocksCPURequest    string
	KVRocksMemoryRequest string
	KVRocksCPULimit      string
	KVRocksMemoryLimit   string
	ArtifactsEnabled     bool
	ArtifactsSize        string
	RequeueInspectLimit  int
	EmbeddedKVRocks      bool
	DevAuth              bool
}

type installBundle struct {
	OutputDir    string
	Files        []string
	BuildCommand []string
	Command      []string
	Warnings     []string
}

type loginConfig struct {
	URLTemplate  string            `yaml:"urlTemplate"`
	Method       string            `yaml:"method"`
	Headers      map[string]string `yaml:"headers"`
	BodyTemplate string            `yaml:"bodyTemplate"`
	ContentType  string            `yaml:"contentType"`
	TokenPath    string            `yaml:"tokenPath"`
}

type exchangeConfig struct {
	URLTemplate string            `yaml:"urlTemplate"`
	Method      string            `yaml:"method"`
	Headers     map[string]string `yaml:"headers"`
	ContentType string            `yaml:"contentType"`
	TokenPath   string            `yaml:"tokenPath"`

	// Request fields for Tikti token exchange.
	Audience string   `yaml:"audience"`
	TenantID string   `yaml:"tenantId"`
	Scopes   []string `yaml:"scopes"`
}

func newUI() *ui {
	return &ui{
		title: color.New(color.FgHiCyan, color.Bold).SprintFunc(),
		ok:    color.New(color.FgGreen, color.Bold).SprintFunc(),
		info:  color.New(color.FgCyan).SprintFunc(),
		warn:  color.New(color.FgYellow).SprintFunc(),
		err:   color.New(color.FgRed, color.Bold).SprintFunc(),
		dim:   color.New(color.FgHiBlack).SprintFunc(),
	}
}

func (c *client) request(method, path string, token string, body any) (int, []byte, error) {
	var buf *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		buf = bytes.NewReader(b)
	} else {
		buf = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, c.baseURL+path, buf)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if c.admin {
		req.Header.Set("X-Role", "ADMIN")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out, nil
}

func main() {
	baseURL := getenv("CODEQ_BASE_URL", "http://localhost:8080")
	producerToken := getenv("CODEQ_PRODUCER_TOKEN", "producer-token")
	workerToken := getenv("CODEQ_WORKER_TOKEN", "")
	admin := getenvBool("CODEQ_ADMIN", isLocalURL(baseURL))
	profileName := getenv("CODEQ_PROFILE", "")
	iamBaseURL := getenv("CODEQ_IAM_BASE_URL", defaultIAMBaseURL)
	iamAPIKey := getenv("CODEQ_IAM_API_KEY", "")
	ui := newUI()

	root := &cobra.Command{
		Use:   "codeq",
		Short: "codeQ CLI",
		Long:  "codeQ CLI for scheduling, workers, and queue operations.",
	}
	root.SetHelpTemplate(helpTemplate(ui))
	root.SilenceUsage = true

	root.PersistentFlags().StringVar(&baseURL, "base-url", baseURL, "Base URL for codeQ")
	root.PersistentFlags().StringVar(&iamBaseURL, "iam-base-url", iamBaseURL, "IAM base URL")
	root.PersistentFlags().StringVar(&iamAPIKey, "iam-api-key", iamAPIKey, "IAM API key")
	root.PersistentFlags().StringVar(&producerToken, "producer-token", producerToken, "Producer token")
	root.PersistentFlags().StringVar(&workerToken, "worker-token", workerToken, "Worker token (JWT)")
	root.PersistentFlags().BoolVar(&admin, "admin", admin, "Send X-Role: ADMIN (dev only)")
	root.PersistentFlags().StringVar(&profileName, "profile", profileName, "Config profile")

	root.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		cfg, _, _ := loadConfig()
		active := resolveProfileName(profileName, cfg)
		prof := cfg.Profiles[active]

		flags := cmd.Flags()
		if !flags.Changed("base-url") {
			if v := strings.TrimSpace(os.Getenv("CODEQ_BASE_URL")); v != "" {
				baseURL = v
			} else if prof.BaseURL != "" {
				baseURL = prof.BaseURL
			}
		}
		if !flags.Changed("iam-base-url") {
			if v := strings.TrimSpace(os.Getenv("CODEQ_IAM_BASE_URL")); v != "" {
				iamBaseURL = v
			} else if prof.IAMBaseURL != "" {
				iamBaseURL = prof.IAMBaseURL
			}
		}
		if !flags.Changed("iam-api-key") {
			if v := strings.TrimSpace(os.Getenv("CODEQ_IAM_API_KEY")); v != "" {
				iamAPIKey = v
			} else if prof.IAMAPIKey != "" {
				iamAPIKey = prof.IAMAPIKey
			}
		}
		if prof.Auth.Login.URLTemplate == "" {
			prof.Auth.Login = defaultLoginConfig(prof.IAMBaseURL, prof.IAMAPIKey)
		}
		if prof.Auth.Exchange.URLTemplate == "" {
			prof.Auth.Exchange = defaultExchangeConfig(prof.IAMBaseURL, prof.IAMAPIKey)
		}
		if !flags.Changed("producer-token") {
			if v := strings.TrimSpace(os.Getenv("CODEQ_PRODUCER_TOKEN")); v != "" {
				producerToken = v
			} else if prof.ProducerToken != "" {
				producerToken = prof.ProducerToken
			} else if prof.Token != "" {
				producerToken = prof.Token
			}
		}
		if !flags.Changed("worker-token") {
			if v := strings.TrimSpace(os.Getenv("CODEQ_WORKER_TOKEN")); v != "" {
				workerToken = v
			} else if prof.WorkerToken != "" {
				workerToken = prof.WorkerToken
			} else if prof.Token != "" {
				workerToken = prof.Token
			}
		}
		if !flags.Changed("admin") {
			if v := strings.TrimSpace(os.Getenv("CODEQ_ADMIN")); v != "" {
				admin = getenvBool("CODEQ_ADMIN", admin)
			} else if prof.Admin {
				admin = true
			} else if isLocalURL(baseURL) {
				admin = true
			}
		}
		if !flags.Changed("profile") && profileName == "" && active != "" {
			profileName = active
		}
		return nil
	}

	root.AddCommand(initCmd(&profileName, &iamBaseURL, &iamAPIKey, ui))
	root.AddCommand(authCmd(&profileName, &iamBaseURL, &iamAPIKey, ui))
	root.AddCommand(taskCmd(&baseURL, &producerToken, &workerToken, &admin, ui))
	root.AddCommand(workerCmd(&baseURL, &producerToken, &workerToken, &admin, ui))
	root.AddCommand(queueCmd(&baseURL, &producerToken, &workerToken, &admin, ui))
	root.AddCommand(installCmd(ui))
	root.AddCommand(migrateCmd(ui))

	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, ui.err("[ERROR]"), err.Error())
		os.Exit(1)
	}
}

func initCmd(profileName *string, iamBaseURL *string, iamAPIKey *string, ui *ui) *cobra.Command {
	var (
		baseURL       string
		iamURL        string
		iamKey        string
		producerToken string
		workerToken   string
		admin         bool
		noPrompt      bool
	)
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize CLI config",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cfgPath, err := loadConfig()
			if err != nil {
				return err
			}
			active := resolveProfileName(*profileName, cfg)
			prof := cfg.Profiles[active]

			if baseURL == "" {
				baseURL = prof.BaseURL
			}
			if baseURL == "" {
				baseURL = "http://localhost:8080"
			}
			if iamURL == "" {
				iamURL = prof.IAMBaseURL
			}
			if iamURL == "" {
				iamURL = *iamBaseURL
			}
			if iamURL == "" {
				iamURL = defaultIAMBaseURL
			}
			if iamKey == "" {
				iamKey = prof.IAMAPIKey
			}
			if iamKey == "" {
				iamKey = *iamAPIKey
			}

			if !noPrompt {
				reader := bufio.NewReader(os.Stdin)
				baseURL = prompt(reader, "Base URL", baseURL)
				iamURL = prompt(reader, "IAM Base URL", iamURL)
				iamKey = prompt(reader, "IAM API Key", iamKey)
				if producerToken == "" {
					producerToken = prompt(reader, "Producer token (optional)", "")
				}
				if workerToken == "" {
					workerToken = prompt(reader, "Worker token (optional)", "")
				}
			}

			prof.BaseURL = strings.TrimSpace(baseURL)
			prof.IAMBaseURL = strings.TrimSpace(iamURL)
			prof.IAMAPIKey = strings.TrimSpace(iamKey)
			if prof.Auth.Login.URLTemplate == "" {
				prof.Auth.Login = defaultLoginConfig(prof.IAMBaseURL, prof.IAMAPIKey)
			}
			if prof.Auth.Exchange.URLTemplate == "" {
				prof.Auth.Exchange = defaultExchangeConfig(prof.IAMBaseURL, prof.IAMAPIKey)
			}
			if producerToken != "" {
				prof.ProducerToken = strings.TrimSpace(producerToken)
			}
			if workerToken != "" {
				prof.WorkerToken = strings.TrimSpace(workerToken)
			}
			if cmd.Flags().Changed("admin") {
				prof.Admin = admin
			}

			if cfg.Profiles == nil {
				cfg.Profiles = map[string]profile{}
			}
			cfg.Profiles[active] = prof
			if cfg.CurrentProfile == "" || *profileName != "" {
				cfg.CurrentProfile = active
			}

			if err := saveConfig(cfg, cfgPath); err != nil {
				return err
			}
			fmt.Printf("%s Initialized profile '%s' at %s\n", ui.ok("[OK]"), active, cfgPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&baseURL, "base-url", "", "Base URL for codeQ")
	cmd.Flags().StringVar(&iamURL, "iam-base-url", "", "IAM base URL")
	cmd.Flags().StringVar(&iamKey, "iam-api-key", "", "IAM API key")
	cmd.Flags().StringVar(&producerToken, "producer-token", "", "Producer token")
	cmd.Flags().StringVar(&workerToken, "worker-token", "", "Worker token (JWT)")
	cmd.Flags().BoolVar(&admin, "admin", false, "Set admin for profile")
	cmd.Flags().BoolVar(&noPrompt, "no-prompt", false, "Disable interactive prompts")
	return cmd
}

func authCmd(profileName *string, iamBaseURL *string, iamAPIKey *string, ui *ui) *cobra.Command {
	auth := &cobra.Command{
		Use:   "auth",
		Short: "Manage stored credentials",
	}

	var (
		producerToken string
		workerToken   string
		admin         bool
		clearAll      bool
	)

	set := &cobra.Command{
		Use:   "set",
		Short: "Store tokens in config",
		RunE: func(cmd *cobra.Command, args []string) error {
			if producerToken == "" && workerToken == "" && !cmd.Flags().Changed("admin") {
				return errors.New("provide --producer-token and/or --worker-token (or --admin)")
			}
			cfg, cfgPath, err := loadConfig()
			if err != nil {
				return err
			}
			active := resolveProfileName(*profileName, cfg)
			prof := cfg.Profiles[active]
			if producerToken != "" {
				prof.ProducerToken = strings.TrimSpace(producerToken)
			}
			if workerToken != "" {
				prof.WorkerToken = strings.TrimSpace(workerToken)
			}
			if producerToken != "" && workerToken == "" {
				prof.Token = strings.TrimSpace(producerToken)
			}
			if workerToken != "" && producerToken == "" {
				prof.Token = strings.TrimSpace(workerToken)
			}
			if cmd.Flags().Changed("admin") {
				prof.Admin = admin
			}
			if cfg.Profiles == nil {
				cfg.Profiles = map[string]profile{}
			}
			cfg.Profiles[active] = prof
			if cfg.CurrentProfile == "" || *profileName != "" {
				cfg.CurrentProfile = active
			}
			if err := saveConfig(cfg, cfgPath); err != nil {
				return err
			}
			fmt.Printf("%s Credentials updated for '%s'\n", ui.ok("[OK]"), active)
			return nil
		},
	}
	set.Flags().StringVar(&producerToken, "producer-token", "", "Producer token")
	set.Flags().StringVar(&workerToken, "worker-token", "", "Worker token (JWT)")
	set.Flags().BoolVar(&admin, "admin", false, "Set admin for profile")

	var (
		loginEmail        string
		loginPassword     string
		loginURL          string
		loginMethod       string
		loginCT           string
		loginPayload      string
		loginPayloadFile  string
		loginTokenPath    string
		saveLoginConfig   bool
		headerKVs         []string
		exchangeURL       string
		exchangeMethod    string
		exchangeCT        string
		exchangeTokenPath string
		exchangeAudience  string
		exchangeTenantID  string
		exchangeScopes    []string
		exchangeHeaderKVs []string
		noExchange        bool
		noPrompt          bool
	)
	login := &cobra.Command{
		Use:   "login",
		Short: "Login via IAM, exchange idToken for accessToken, and store it",
		RunE: func(cmd *cobra.Command, args []string) error {
			email := strings.TrimSpace(loginEmail)
			password := strings.TrimSpace(loginPassword)
			if email == "" && !noPrompt {
				reader := bufio.NewReader(os.Stdin)
				email = prompt(reader, "Email", "")
			}
			if password == "" && !noPrompt {
				p, err := promptSecret("Password")
				if err != nil {
					return err
				}
				password = p
			}
			if email == "" || password == "" {
				return errors.New("email and password are required")
			}

			cfg, cfgPath, err := loadConfig()
			if err != nil {
				return err
			}
			active := resolveProfileName(*profileName, cfg)
			if *profileName == "" {
				active = profileFromEmail(email)
			}
			prof := cfg.Profiles[active]
			if prof.IAMBaseURL == "" {
				prof.IAMBaseURL = *iamBaseURL
			}
			if prof.IAMAPIKey == "" {
				prof.IAMAPIKey = *iamAPIKey
			}

			loginCfg := prof.Auth.Login
			if loginCfg.URLTemplate == "" {
				loginCfg = defaultLoginConfig(prof.IAMBaseURL, prof.IAMAPIKey)
			}
			if strings.TrimSpace(loginURL) != "" {
				loginCfg.URLTemplate = loginURL
			}
			if strings.TrimSpace(loginMethod) != "" {
				loginCfg.Method = loginMethod
			}
			if strings.TrimSpace(loginCT) != "" {
				loginCfg.ContentType = loginCT
			}
			if strings.TrimSpace(loginTokenPath) != "" {
				loginCfg.TokenPath = loginTokenPath
			}
			if strings.TrimSpace(loginPayload) != "" {
				loginCfg.BodyTemplate = loginPayload
			}
			if strings.TrimSpace(loginPayloadFile) != "" {
				// #nosec G304 -- the operator explicitly selects this same-user CLI input file.
				data, err := os.ReadFile(loginPayloadFile)
				if err != nil {
					return err
				}
				loginCfg.BodyTemplate = string(data)
			}
			if len(headerKVs) > 0 {
				if loginCfg.Headers == nil {
					loginCfg.Headers = map[string]string{}
				}
				for _, kv := range headerKVs {
					k, v, ok := strings.Cut(kv, ":")
					if !ok {
						return fmt.Errorf("invalid header: %s (expected Key: Value)", kv)
					}
					loginCfg.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}

			exchangeCfg := prof.Auth.Exchange
			if exchangeCfg.URLTemplate == "" {
				exchangeCfg = defaultExchangeConfig(prof.IAMBaseURL, prof.IAMAPIKey)
			}
			if strings.TrimSpace(exchangeURL) != "" {
				exchangeCfg.URLTemplate = exchangeURL
			}
			if strings.TrimSpace(exchangeMethod) != "" {
				exchangeCfg.Method = exchangeMethod
			}
			if strings.TrimSpace(exchangeCT) != "" {
				exchangeCfg.ContentType = exchangeCT
			}
			if strings.TrimSpace(exchangeTokenPath) != "" {
				exchangeCfg.TokenPath = exchangeTokenPath
			}
			if strings.TrimSpace(exchangeAudience) != "" {
				exchangeCfg.Audience = exchangeAudience
			}
			if strings.TrimSpace(exchangeTenantID) != "" {
				exchangeCfg.TenantID = exchangeTenantID
			}
			if len(exchangeScopes) > 0 {
				exchangeCfg.Scopes = exchangeScopes
			}
			if len(exchangeHeaderKVs) > 0 {
				if exchangeCfg.Headers == nil {
					exchangeCfg.Headers = map[string]string{}
				}
				for _, kv := range exchangeHeaderKVs {
					k, v, ok := strings.Cut(kv, ":")
					if !ok {
						return fmt.Errorf("invalid exchange header: %s (expected Key: Value)", kv)
					}
					exchangeCfg.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}

			idToken, err := iamLoginGeneric(loginCfg, prof.IAMBaseURL, prof.IAMAPIKey, email, password)
			if err != nil {
				return err
			}
			token := idToken
			if !noExchange {
				accessToken, err := iamExchange(exchangeCfg, prof.IAMBaseURL, prof.IAMAPIKey, idToken)
				if err != nil {
					return err
				}
				token = accessToken
			}

			prof.Token = token
			prof.ProducerToken = token

			if cfg.Profiles == nil {
				cfg.Profiles = map[string]profile{}
			}
			if saveLoginConfig {
				prof.Auth.Login = loginCfg
				prof.Auth.Exchange = exchangeCfg
			}
			cfg.Profiles[active] = prof
			cfg.CurrentProfile = active
			if err := saveConfig(cfg, cfgPath); err != nil {
				return err
			}
			fmt.Printf("%s Logged in. Token stored for '%s'\n", ui.ok("[OK]"), active)
			return nil
		},
	}
	login.Flags().StringVar(&loginEmail, "email", "", "Email for login")
	login.Flags().StringVar(&loginPassword, "password", "", "Password for login")
	login.Flags().StringVar(&loginURL, "login-url", "", "Override IAM login URL (template allowed)")
	login.Flags().StringVar(&loginMethod, "method", "", "HTTP method (default POST)")
	login.Flags().StringVar(&loginCT, "content-type", "", "Content-Type override")
	login.Flags().StringVar(&loginPayload, "payload", "", "Login payload (template allowed)")
	login.Flags().StringVar(&loginPayloadFile, "payload-file", "", "Login payload file (template allowed)")
	login.Flags().StringVar(&loginTokenPath, "token-path", "", "JSON token path (default idToken)")
	login.Flags().StringArrayVar(&headerKVs, "header", nil, "Extra headers (Key: Value)")
	login.Flags().StringVar(&exchangeURL, "exchange-url", "", "Override IAM token exchange URL (template allowed)")
	login.Flags().StringVar(&exchangeMethod, "exchange-method", "", "Token exchange HTTP method (default POST)")
	login.Flags().StringVar(&exchangeCT, "exchange-content-type", "", "Token exchange Content-Type override")
	login.Flags().StringVar(&exchangeTokenPath, "exchange-token-path", "", "Token exchange JSON token path (default accessToken)")
	login.Flags().StringVar(&exchangeAudience, "audience", "", "Access token audience for exchange")
	login.Flags().StringVar(&exchangeTenantID, "tenant-id", "", "Tenant ID for exchange (optional)")
	login.Flags().StringArrayVar(&exchangeScopes, "scope", nil, "Requested scopes for exchange (repeatable)")
	login.Flags().StringArrayVar(&exchangeHeaderKVs, "exchange-header", nil, "Extra token exchange headers (Key: Value)")
	login.Flags().BoolVar(&noExchange, "no-exchange", false, "Skip token exchange and store idToken (dev only)")
	login.Flags().BoolVar(&saveLoginConfig, "save", true, "Save login/exchange config for this profile")
	login.Flags().BoolVar(&noPrompt, "no-prompt", false, "Disable interactive prompts")

	show := &cobra.Command{
		Use:   "show",
		Short: "Show stored credentials (masked)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := loadConfig()
			if err != nil {
				return err
			}
			active := resolveProfileName(*profileName, cfg)
			prof := cfg.Profiles[active]
			loginCfg := prof.Auth.Login
			if loginCfg.URLTemplate == "" {
				loginCfg = defaultLoginConfig(prof.IAMBaseURL, prof.IAMAPIKey)
			}
			exchangeCfg := prof.Auth.Exchange
			if exchangeCfg.URLTemplate == "" {
				exchangeCfg = defaultExchangeConfig(prof.IAMBaseURL, prof.IAMAPIKey)
			}

			fmt.Printf("%s Profile: %s\n", ui.title("codeq"), active)
			fmt.Printf("%s Base URL: %s\n", ui.info("•"), emptyOr(prof.BaseURL, "<unset>"))
			fmt.Printf("%s IAM URL:  %s\n", ui.info("•"), emptyOr(prof.IAMBaseURL, "<unset>"))
			fmt.Printf("%s API Key:  %s\n", ui.info("•"), maskToken(prof.IAMAPIKey))
			fmt.Printf("%s Login URL: %s\n", ui.info("•"), emptyOr(loginCfg.URLTemplate, "<unset>"))
			fmt.Printf("%s Token Path: %s\n", ui.info("•"), emptyOr(loginCfg.TokenPath, "<unset>"))
			fmt.Printf("%s Exchange URL: %s\n", ui.info("•"), emptyOr(exchangeCfg.URLTemplate, "<unset>"))
			fmt.Printf("%s Exchange Token Path: %s\n", ui.info("•"), emptyOr(exchangeCfg.TokenPath, "<unset>"))
			fmt.Printf("%s Audience: %s\n", ui.info("•"), emptyOr(exchangeCfg.Audience, "<unset>"))
			fmt.Printf("%s Token:    %s\n", ui.info("•"), maskToken(firstNonEmpty(prof.Token, prof.ProducerToken, prof.WorkerToken)))
			fmt.Printf("%s Admin:    %v\n", ui.info("•"), prof.Admin)
			return nil
		},
	}

	clear := &cobra.Command{
		Use:   "clear",
		Short: "Clear stored tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cfgPath, err := loadConfig()
			if err != nil {
				return err
			}
			active := resolveProfileName(*profileName, cfg)
			prof := cfg.Profiles[active]

			if clearAll || (!cmd.Flags().Changed("producer") && !cmd.Flags().Changed("worker")) {
				prof.ProducerToken = ""
				prof.WorkerToken = ""
				prof.Token = ""
			} else {
				if cmd.Flags().Changed("producer") {
					prof.ProducerToken = ""
					prof.Token = ""
				}
				if cmd.Flags().Changed("worker") {
					prof.WorkerToken = ""
					prof.Token = ""
				}
			}
			cfg.Profiles[active] = prof
			if err := saveConfig(cfg, cfgPath); err != nil {
				return err
			}
			fmt.Printf("%s Tokens cleared for '%s'\n", ui.ok("[OK]"), active)
			return nil
		},
	}
	clear.Flags().Bool("producer", false, "Clear producer token")
	clear.Flags().Bool("worker", false, "Clear worker token")
	clear.Flags().BoolVar(&clearAll, "all", false, "Clear all tokens")

	auth.AddCommand(login, set, show, clear)
	return auth
}

func taskCmd(baseURL, producerToken, workerToken *string, admin *bool, ui *ui) *cobra.Command {
	task := &cobra.Command{
		Use:   "task",
		Short: "Task operations",
	}

	var (
		event          string
		payload        string
		priority       int
		webhook        string
		maxAttempts    int
		idempotencyKey string
		runAt          string
		delaySeconds   int
	)

	create := &cobra.Command{
		Use:     "create",
		Short:   "Create a task",
		Example: "codeq task create --event render_video --priority 10 --payload '{\"jobId\":500}'",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(event) == "" {
				return errors.New("event is required")
			}
			if strings.TrimSpace(payload) == "" {
				return errors.New("payload is required")
			}
			var payloadObj any
			if err := json.Unmarshal([]byte(payload), &payloadObj); err != nil {
				return fmt.Errorf("invalid payload JSON: %w", err)
			}

			token := producerAuthToken(*producerToken, *workerToken)
			if token == "" {
				return errors.New("token is required (run `codeq auth login` or set token)")
			}
			c := newClient(*baseURL, *producerToken, *workerToken, *admin)
			body := map[string]any{
				"command":  event,
				"payload":  payloadObj,
				"priority": priority,
			}
			if webhook != "" {
				body["webhook"] = webhook
			}
			if maxAttempts > 0 {
				body["maxAttempts"] = maxAttempts
			}
			if idempotencyKey != "" {
				body["idempotencyKey"] = idempotencyKey
			}
			if runAt != "" {
				body["runAt"] = runAt
			}
			if delaySeconds > 0 {
				body["delaySeconds"] = delaySeconds
			}

			spin := spinner.New(spinner.CharSets[14], 120*time.Millisecond)
			spin.Suffix = " Enqueueing task..."
			spin.Start()
			status, resp, err := c.request("POST", "/v1/codeq/tasks", token, body)
			spin.Stop()
			if err != nil {
				return err
			}
			if status >= 300 {
				return fmt.Errorf("error (%d): %s", status, string(resp))
			}
			var out taskResp
			if err := json.Unmarshal(resp, &out); err != nil {
				fmt.Println(string(resp))
				return nil
			}
			fmt.Printf("%s Task created: %s\n", ui.ok("[OK]"), out.ID)
			return nil
		},
	}
	create.Flags().StringVar(&event, "event", "", "Event/command name")
	create.Flags().StringVar(&payload, "payload", "", "JSON payload")
	create.Flags().IntVar(&priority, "priority", 0, "Priority (0-9)")
	create.Flags().StringVar(&webhook, "webhook", "", "Result webhook URL")
	create.Flags().IntVar(&maxAttempts, "max-attempts", 0, "Max attempts")
	create.Flags().StringVar(&idempotencyKey, "idempotency-key", "", "Idempotency key")
	create.Flags().StringVar(&runAt, "run-at", "", "RFC3339 timestamp when the task becomes visible to workers")
	create.Flags().IntVar(&delaySeconds, "delay-seconds", 0, "Delay in seconds before the task becomes visible to workers")

	get := &cobra.Command{
		Use:   "get <id>",
		Short: "Get a task",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			token := producerAuthToken(*producerToken, *workerToken)
			if token == "" {
				return errors.New("token is required (run `codeq auth login` or set token)")
			}
			c := newClient(*baseURL, *producerToken, *workerToken, *admin)
			spin := spinner.New(spinner.CharSets[14], 120*time.Millisecond)
			spin.Suffix = " Fetching task..."
			spin.Start()
			status, resp, err := c.request("GET", "/v1/codeq/tasks/"+url.PathEscape(id), token, nil)
			spin.Stop()
			if err != nil {
				return err
			}
			if status >= 300 {
				return fmt.Errorf("error (%d): %s", status, string(resp))
			}
			fmt.Println(string(resp))
			return nil
		},
	}

	result := &cobra.Command{
		Use:   "result <id>",
		Short: "Get a task result",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := args[0]
			token := producerAuthToken(*producerToken, *workerToken)
			if token == "" {
				return errors.New("token is required (run `codeq auth login` or set token)")
			}
			c := newClient(*baseURL, *producerToken, *workerToken, *admin)
			spin := spinner.New(spinner.CharSets[14], 120*time.Millisecond)
			spin.Suffix = " Fetching result..."
			spin.Start()
			status, resp, err := c.request("GET", "/v1/codeq/tasks/"+url.PathEscape(id)+"/result", token, nil)
			spin.Stop()
			if err != nil {
				return err
			}
			if status >= 300 {
				return fmt.Errorf("error (%d): %s", status, string(resp))
			}
			fmt.Println(string(resp))
			return nil
		},
	}

	task.AddCommand(create, get, result)
	return task
}

func workerCmd(baseURL, producerToken, workerToken *string, admin *bool, ui *ui) *cobra.Command {
	var (
		events      string
		concurrency int
		leaseSec    int
		waitSec     int
		ackMode     string
		nackDelay   int
	)

	start := &cobra.Command{
		Use:     "start",
		Short:   "Start a worker (polling)",
		Example: "codeq worker start --events render_video --concurrency 5",
		RunE: func(cmd *cobra.Command, args []string) error {
			tokenSource := "worker"
			token := strings.TrimSpace(*workerToken)
			if token == "" && strings.TrimSpace(*producerToken) != "" {
				tokenSource = "producer"
				token = strings.TrimSpace(*producerToken)
			}
			if token == "" {
				return errors.New("token is required (run `codeq auth login` or set token)")
			}
			ev := splitEvents(events)
			if len(ev) == 0 {
				return errors.New("events are required")
			}
			if concurrency <= 0 {
				concurrency = 1
			}
			if leaseSec <= 0 {
				leaseSec = 60
			}
			if waitSec < 0 {
				waitSec = 0
			}
			switch ackMode {
			case "complete", "abandon", "nack", "none":
			default:
				return errors.New("ack must be one of: complete, abandon, nack, none")
			}

			c := newClient(*baseURL, *producerToken, token, *admin)
			bar := progressbar.NewOptions(concurrency,
				progressbar.OptionSetDescription("Starting worker pool"),
				progressbar.OptionSetWidth(18),
				progressbar.OptionShowCount(),
				progressbar.OptionClearOnFinish(),
			)
			for i := 0; i < concurrency; i++ {
				_ = bar.Add(1)
			}
			fmt.Printf("%s Worker connected. Listening... (token: %s)\n", ui.info("[INFO]"), tokenSource)

			ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer cancel()

			var wg sync.WaitGroup
			for i := 0; i < concurrency; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					workerLoop(ctx, c, ev, leaseSec, waitSec, ackMode, nackDelay, ui)
				}()
			}

			<-ctx.Done()
			fmt.Println(ui.warn("[WARN]"), "Stopping...")
			wg.Wait()
			return nil
		},
	}

	start.Flags().StringVar(&events, "events", "", "Comma-separated event list")
	start.Flags().IntVar(&concurrency, "concurrency", 1, "Number of worker threads")
	start.Flags().IntVar(&leaseSec, "lease-seconds", 60, "Lease seconds")
	start.Flags().IntVar(&waitSec, "wait-seconds", 10, "Long-poll wait seconds (0-30)")
	start.Flags().StringVar(&ackMode, "ack", "abandon", "Ack mode: complete|abandon|nack|none")
	start.Flags().IntVar(&nackDelay, "nack-delay", 5, "Delay seconds for nack")

	cmd := &cobra.Command{
		Use:   "worker",
		Short: "Worker operations",
	}
	cmd.AddCommand(start)
	return cmd
}

func queueCmd(baseURL, producerToken, workerToken *string, admin *bool, ui *ui) *cobra.Command {
	inspect := &cobra.Command{
		Use:     "inspect <event>",
		Short:   "Inspect queue depth",
		Example: "codeq queue inspect render_video",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			event := args[0]
			token := producerAuthToken(*producerToken, *workerToken)
			if token == "" {
				return errors.New("token is required (run `codeq auth login` or set token)")
			}
			c := newClient(*baseURL, *producerToken, *workerToken, *admin)
			spin := spinner.New(spinner.CharSets[14], 120*time.Millisecond)
			spin.Suffix = " Inspecting queue..."
			spin.Start()
			status, resp, err := c.request("GET", "/v1/codeq/admin/queues/"+url.PathEscape(event), token, nil)
			spin.Stop()
			if err != nil {
				return err
			}
			if status >= 300 {
				return fmt.Errorf("error (%d): %s", status, string(resp))
			}
			var out queueStats
			if err := json.Unmarshal(resp, &out); err != nil {
				fmt.Println(string(resp))
				return nil
			}
			fmt.Printf("%s: %d | %s: %d | %s: %d | %s: %d\n",
				ui.ok("READY"), out.Ready,
				ui.warn("DELAYED"), out.Delayed,
				ui.info("IN_PROGRESS"), out.InProgress,
				ui.err("DLQ"), out.DLQ,
			)
			return nil
		},
	}

	cmd := &cobra.Command{
		Use:   "queue",
		Short: "Queue operations",
	}
	cmd.AddCommand(inspect)
	return cmd
}

func installCmd(ui *ui) *cobra.Command {
	opts := installOptions{
		Target:             "",
		Size:               "small",
		Release:            "codeq",
		Namespace:          "codeq",
		Chart:              "./helm/codeq",
		OutputDir:          "codeq-install",
		Image:              "ghcr.io/osvaldoandrade/codeq-service:latest",
		IdentityServiceURL: "https://issuer.example.com",
		WorkerJwksURL:      "https://issuer.example.com/.well-known/jwks.json",
		WorkerIssuer:       "https://issuer.example.com",
		WorkerAudience:     "codeq-worker",
	}

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Generate a Docker or Helm install bundle",
		Long: "Generate a Docker Compose or Kubernetes/Helm installation bundle for codeQ.\n" +
			"By default this command writes files and prints the next command. Use --execute to run it.",
		RunE: func(cmd *cobra.Command, args []string) error {
			sizeChanged := cmd.Flags().Changed("size")
			targetChanged := cmd.Flags().Changed("target")
			imageChanged := cmd.Flags().Changed("image")
			if !targetChanged && opts.Target == "" {
				opts.Target = "docker"
			}
			opts.Target = normalizeInstallTarget(opts.Target)
			if !sizeChanged {
				if opts.Target == "docker" {
					opts.Size = "dev"
				} else {
					opts.Size = "small"
				}
			}

			if !opts.NoPrompt && isTerminal(int(os.Stdin.Fd())) {
				if err := runInstallWizard(&opts); err != nil {
					return err
				}
			}

			opts.Target = normalizeInstallTarget(opts.Target)
			opts.Size = strings.ToLower(strings.TrimSpace(opts.Size))
			profile, err := installProfileFor(opts.Size)
			if err != nil {
				return err
			}
			if opts.Target != "docker" && opts.Target != "kubernetes" {
				return errors.New("target must be one of: docker, kubernetes")
			}
			if opts.Build {
				if opts.Target != "docker" {
					return errors.New("--build is only supported with --target docker")
				}
				if !imageChanged {
					opts.Image = "codeq-service:local"
				}
			}
			if strings.TrimSpace(opts.WebhookHmacSecret) == "" {
				secret, err := randomHex(32)
				if err != nil {
					return err
				}
				opts.WebhookHmacSecret = secret
			}

			bundle, err := writeInstallBundle(opts, profile)
			if err != nil {
				return err
			}
			printInstallBundle(bundle, ui)

			if opts.Execute {
				if err := validateExecutableInstall(opts, profile); err != nil {
					return err
				}
				if len(bundle.BuildCommand) > 0 {
					fmt.Printf("%s Running: %s\n", ui.info("[INFO]"), shellJoin(bundle.BuildCommand))
					if err := runInstallCommand(cmd.Context(), bundle.BuildCommand); err != nil {
						return err
					}
				}
				fmt.Printf("%s Running: %s\n", ui.info("[INFO]"), shellJoin(bundle.Command))
				if err := runInstallCommand(cmd.Context(), bundle.Command); err != nil {
					return err
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&opts.Target, "target", opts.Target, "Install target: docker|kubernetes")
	cmd.Flags().StringVar(&opts.Size, "size", opts.Size, "Install size: dev|small|medium|large")
	cmd.Flags().StringVar(&opts.Release, "release", opts.Release, "Helm release name")
	cmd.Flags().StringVar(&opts.Namespace, "namespace", opts.Namespace, "Kubernetes namespace")
	cmd.Flags().StringVar(&opts.Chart, "chart", opts.Chart, "Helm chart path or reference")
	cmd.Flags().StringVar(&opts.OutputDir, "output-dir", opts.OutputDir, "Directory for generated install files")
	cmd.Flags().StringVar(&opts.Image, "image", opts.Image, "codeQ server image")
	cmd.Flags().StringVar(&opts.Domain, "domain", opts.Domain, "Ingress host for Kubernetes")
	cmd.Flags().StringVar(&opts.RedisAddr, "redis-addr", opts.RedisAddr, "External Redis/KVRocks address")
	cmd.Flags().StringVar(&opts.IdentityServiceURL, "identity-service-url", opts.IdentityServiceURL, "Producer identity service base URL")
	cmd.Flags().StringVar(&opts.WorkerJwksURL, "worker-jwks-url", opts.WorkerJwksURL, "Worker JWKS URL")
	cmd.Flags().StringVar(&opts.WorkerIssuer, "worker-issuer", opts.WorkerIssuer, "Worker token issuer")
	cmd.Flags().StringVar(&opts.WorkerAudience, "worker-audience", opts.WorkerAudience, "Worker token audience")
	cmd.Flags().StringVar(&opts.WebhookHmacSecret, "webhook-hmac-secret", opts.WebhookHmacSecret, "Webhook HMAC secret; generated when empty")
	cmd.Flags().BoolVar(&opts.Build, "build", false, "Build the codeQ server image locally with docker build before launching (target=docker)")
	cmd.Flags().StringVar(&opts.BuildContext, "build-context", "", "Docker build context (defaults to repo root)")
	cmd.Flags().BoolVar(&opts.Execute, "execute", false, "Run docker compose or helm after generating files")
	cmd.Flags().BoolVar(&opts.NoPrompt, "no-prompt", false, "Disable interactive wizard prompts")
	return cmd
}

func installProfiles() map[string]installProfile {
	return map[string]installProfile{
		"dev": {
			Name:                 "dev",
			Description:          "single-node development stack with static dev tokens",
			Replicas:             1,
			MinReplicas:          1,
			MaxReplicas:          1,
			TargetCPU:            80,
			CPURequest:           "100m",
			MemoryRequest:        "256Mi",
			CPULimit:             "500m",
			MemoryLimit:          "512Mi",
			KVRocksSize:          "2Gi",
			KVRocksCPURequest:    "100m",
			KVRocksMemoryRequest: "256Mi",
			KVRocksCPULimit:      "500m",
			KVRocksMemoryLimit:   "512Mi",
			ArtifactsSize:        "1Gi",
			RequeueInspectLimit:  200,
			EmbeddedKVRocks:      true,
			DevAuth:              true,
		},
		"small": {
			Name:                 "small",
			Description:          "small production install with embedded KVRocks",
			Replicas:             2,
			MinReplicas:          2,
			MaxReplicas:          4,
			TargetCPU:            75,
			CPURequest:           "250m",
			MemoryRequest:        "512Mi",
			CPULimit:             "1",
			MemoryLimit:          "1Gi",
			KVRocksSize:          "20Gi",
			KVRocksCPURequest:    "250m",
			KVRocksMemoryRequest: "512Mi",
			KVRocksCPULimit:      "1",
			KVRocksMemoryLimit:   "1Gi",
			ArtifactsSize:        "5Gi",
			RequeueInspectLimit:  200,
			EmbeddedKVRocks:      true,
		},
		"medium": {
			Name:                "medium",
			Description:         "multi-replica production install; external KVRocks recommended",
			Replicas:            3,
			MinReplicas:         3,
			MaxReplicas:         8,
			TargetCPU:           70,
			CPURequest:          "500m",
			MemoryRequest:       "1Gi",
			CPULimit:            "2",
			MemoryLimit:         "2Gi",
			KVRocksSize:         "50Gi",
			ArtifactsEnabled:    true,
			ArtifactsSize:       "20Gi",
			RequeueInspectLimit: 500,
		},
		"large": {
			Name:                "large",
			Description:         "larger production install; external KVRocks required",
			Replicas:            5,
			MinReplicas:         5,
			MaxReplicas:         20,
			TargetCPU:           65,
			CPURequest:          "1",
			MemoryRequest:       "2Gi",
			CPULimit:            "4",
			MemoryLimit:         "4Gi",
			KVRocksSize:         "100Gi",
			ArtifactsEnabled:    true,
			ArtifactsSize:       "100Gi",
			RequeueInspectLimit: 1000,
		},
	}
}

func installProfileFor(size string) (installProfile, error) {
	profile, ok := installProfiles()[strings.ToLower(strings.TrimSpace(size))]
	if !ok {
		return installProfile{}, errors.New("size must be one of: dev, small, medium, large")
	}
	return profile, nil
}

func runInstallWizard(opts *installOptions) error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Println("codeQ install wizard")
	opts.Target = promptChoice(reader, "Target (docker/kubernetes)", emptyOr(opts.Target, "docker"), []string{"docker", "kubernetes", "k8s", "helm"})
	opts.Target = normalizeInstallTarget(opts.Target)
	defaultSize := "small"
	if opts.Target == "docker" {
		defaultSize = "dev"
	}
	opts.Size = promptChoice(reader, "Size (dev/small/medium/large)", emptyOr(opts.Size, defaultSize), []string{"dev", "small", "medium", "large"})
	opts.OutputDir = prompt(reader, "Output directory", emptyOr(opts.OutputDir, "codeq-install"))
	if opts.Target == "docker" {
		opts.Build = promptYesNo(reader, "Build server image locally with docker build", opts.Build)
	}
	defaultImage := "ghcr.io/osvaldoandrade/codeq-service:latest"
	if opts.Build {
		defaultImage = "codeq-service:local"
	}
	opts.Image = prompt(reader, "Server image", emptyOr(opts.Image, defaultImage))

	if opts.Target == "kubernetes" {
		opts.Release = prompt(reader, "Helm release", emptyOr(opts.Release, "codeq"))
		opts.Namespace = prompt(reader, "Namespace", emptyOr(opts.Namespace, "codeq"))
		opts.Chart = prompt(reader, "Chart", emptyOr(opts.Chart, "./helm/codeq"))
		opts.Domain = prompt(reader, "Ingress host (optional)", opts.Domain)
	}

	profile, err := installProfileFor(opts.Size)
	if err != nil {
		return err
	}
	if !profile.DevAuth {
		opts.IdentityServiceURL = prompt(reader, "Identity service URL", opts.IdentityServiceURL)
		opts.WorkerJwksURL = prompt(reader, "Worker JWKS URL", opts.WorkerJwksURL)
		opts.WorkerIssuer = prompt(reader, "Worker issuer", opts.WorkerIssuer)
		opts.WorkerAudience = prompt(reader, "Worker audience", emptyOr(opts.WorkerAudience, "codeq-worker"))
	}
	if opts.Target == "kubernetes" || !profile.EmbeddedKVRocks {
		opts.RedisAddr = prompt(reader, "External KVRocks/Redis address (blank for embedded when supported)", opts.RedisAddr)
	}
	if opts.WebhookHmacSecret == "" {
		opts.WebhookHmacSecret = prompt(reader, "Webhook HMAC secret (blank to generate)", "")
	}
	opts.Execute = promptYesNo(reader, "Run install command now", opts.Execute)
	return nil
}

func normalizeInstallTarget(target string) string {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "", "docker", "compose", "docker-compose":
		return "docker"
	case "k8s", "kubernetes", "helm":
		return "kubernetes"
	default:
		return strings.ToLower(strings.TrimSpace(target))
	}
}

func writeInstallBundle(opts installOptions, profile installProfile) (installBundle, error) {
	outDir := filepath.Clean(opts.OutputDir)
	if outDir == "." || outDir == "" {
		outDir = "codeq-install"
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return installBundle{}, err
	}
	if opts.Target == "kubernetes" {
		return writeHelmInstallBundle(opts, profile, outDir)
	}
	return writeDockerInstallBundle(opts, profile, outDir)
}

func writeDockerInstallBundle(opts installOptions, profile installProfile, outDir string) (installBundle, error) {
	composePath := filepath.Join(outDir, "compose.yaml")
	envPath := filepath.Join(outDir, ".env")
	if err := os.WriteFile(composePath, []byte(renderDockerInstallCompose()), 0o600); err != nil {
		return installBundle{}, err
	}
	if err := os.WriteFile(envPath, []byte(renderDockerInstallEnv(opts, profile)), 0o600); err != nil {
		return installBundle{}, err
	}
	bundle := installBundle{
		OutputDir: outDir,
		Files:     []string{composePath, envPath},
		Command:   []string{"docker", "compose", "--env-file", envPath, "-f", composePath, "up", "-d"},
	}
	if opts.Build {
		ctx := strings.TrimSpace(opts.BuildContext)
		if ctx == "" {
			ctx = "."
		}
		bundle.BuildCommand = []string{"docker", "build", "-t", opts.Image, ctx}
	}
	if profile.Name == "medium" || profile.Name == "large" {
		bundle.Warnings = append(bundle.Warnings, "Docker Compose is generated as a single-node server install; use --target kubernetes for multi-replica production.")
	}
	return bundle, nil
}

func writeHelmInstallBundle(opts installOptions, profile installProfile, outDir string) (installBundle, error) {
	valuesPath := filepath.Join(outDir, "values.yaml")
	values, warnings := renderHelmInstallValues(opts, profile)
	if err := os.WriteFile(valuesPath, []byte(values), 0o600); err != nil {
		return installBundle{}, err
	}
	return installBundle{
		OutputDir: outDir,
		Files:     []string{valuesPath},
		Command: []string{
			"helm", "upgrade", "--install", opts.Release, opts.Chart,
			"--namespace", opts.Namespace,
			"--create-namespace",
			"-f", valuesPath,
		},
		Warnings: warnings,
	}, nil
}

func renderDockerInstallCompose() string {
	return `name: codeq

services:
  codeq:
    image: ${CODEQ_IMAGE}
    ports:
      - "${CODEQ_PORT:-8080}:8080"
    environment:
      PORT: "8080"
      ENV: ${CODEQ_ENV}
      LOG_LEVEL: ${CODEQ_LOG_LEVEL}
      LOG_FORMAT: json
      REDIS_ADDR: ${REDIS_ADDR}
      REDIS_PASSWORD: ${REDIS_PASSWORD:-}
      IDENTITY_SERVICE_URL: ${IDENTITY_SERVICE_URL}
      WORKER_JWKS_URL: ${WORKER_JWKS_URL}
      WORKER_ISSUER: ${WORKER_ISSUER}
      WORKER_AUDIENCE: ${WORKER_AUDIENCE}
      WEBHOOK_HMAC_SECRET: ${WEBHOOK_HMAC_SECRET}
      LOCAL_ARTIFACTS_DIR: /var/lib/codeq/artifacts
      DEFAULT_LEASE_SECONDS: ${DEFAULT_LEASE_SECONDS}
      REQUEUE_INSPECT_LIMIT: ${REQUEUE_INSPECT_LIMIT}
      MAX_ATTEMPTS_DEFAULT: ${MAX_ATTEMPTS_DEFAULT}
      BACKOFF_POLICY: ${BACKOFF_POLICY}
      BACKOFF_BASE_SECONDS: ${BACKOFF_BASE_SECONDS}
      BACKOFF_MAX_SECONDS: ${BACKOFF_MAX_SECONDS}
      ALLOW_PRODUCER_AS_WORKER: ${ALLOW_PRODUCER_AS_WORKER}
      PRODUCER_AUTH_PROVIDER: ${PRODUCER_AUTH_PROVIDER:-}
      PRODUCER_AUTH_CONFIG: ${PRODUCER_AUTH_CONFIG:-}
      WORKER_AUTH_PROVIDER: ${WORKER_AUTH_PROVIDER:-}
      WORKER_AUTH_CONFIG: ${WORKER_AUTH_CONFIG:-}
      TRACING_ENABLED: ${TRACING_ENABLED}
      TRACING_SERVICE_NAME: codeq
      TRACING_OTLP_ENDPOINT: ${TRACING_OTLP_ENDPOINT:-}
      TRACING_OTLP_INSECURE: ${TRACING_OTLP_INSECURE}
      TRACING_SAMPLE_RATIO: ${TRACING_SAMPLE_RATIO}
    depends_on:
      - kvrocks
    volumes:
      - codeq-artifacts:/var/lib/codeq/artifacts
    restart: unless-stopped
    networks:
      - codeq

  kvrocks:
    image: apache/kvrocks:2.7.0
    ports:
      - "${KVROCKS_PORT:-6666}:6666"
    volumes:
      - kvrocks-data:/var/lib/kvrocks
    restart: unless-stopped
    networks:
      - codeq

volumes:
  codeq-artifacts:
  kvrocks-data:

networks:
  codeq:
    driver: bridge
`
}

func renderDockerInstallEnv(opts installOptions, profile installProfile) string {
	envMode := "prod"
	logLevel := "info"
	identityURL := opts.IdentityServiceURL
	workerJwksURL := opts.WorkerJwksURL
	workerIssuer := opts.WorkerIssuer
	allowProducerAsWorker := "false"
	producerAuthProvider := ""
	producerAuthConfig := ""
	workerAuthProvider := ""
	workerAuthConfig := ""
	if profile.DevAuth {
		envMode = "dev"
		logLevel = "debug"
		identityURL = "http://api.codecompany.ai"
		workerJwksURL = ""
		workerIssuer = ""
		allowProducerAsWorker = "true"
		producerAuthProvider = "static"
		producerAuthConfig = `{"token":"dev-token","subject":"producer-dev","email":"dev@codeq.local","raw":{"role":"ADMIN"}}`
		workerAuthProvider = "static"
		workerAuthConfig = `{"token":"dev-token","subject":"worker-dev","scopes":["codeq:claim","codeq:heartbeat","codeq:abandon","codeq:nack","codeq:result","codeq:subscribe"],"eventTypes":["*"]}`
	}
	redisAddr := opts.RedisAddr
	if strings.TrimSpace(redisAddr) == "" {
		redisAddr = "kvrocks:6666"
	}

	lines := []string{
		envKV("CODEQ_IMAGE", opts.Image),
		envKV("CODEQ_PORT", "8080"),
		envKV("CODEQ_ENV", envMode),
		envKV("CODEQ_LOG_LEVEL", logLevel),
		envKV("REDIS_ADDR", redisAddr),
		envKV("REDIS_PASSWORD", ""),
		envKV("KVROCKS_PORT", "6666"),
		envKV("IDENTITY_SERVICE_URL", identityURL),
		envKV("WORKER_JWKS_URL", workerJwksURL),
		envKV("WORKER_ISSUER", workerIssuer),
		envKV("WORKER_AUDIENCE", emptyOr(opts.WorkerAudience, "codeq-worker")),
		envKV("WEBHOOK_HMAC_SECRET", opts.WebhookHmacSecret),
		envKV("DEFAULT_LEASE_SECONDS", "300"),
		envKV("REQUEUE_INSPECT_LIMIT", fmt.Sprintf("%d", profile.RequeueInspectLimit)),
		envKV("MAX_ATTEMPTS_DEFAULT", "5"),
		envKV("BACKOFF_POLICY", "exp_full_jitter"),
		envKV("BACKOFF_BASE_SECONDS", "5"),
		envKV("BACKOFF_MAX_SECONDS", "900"),
		envKV("ALLOW_PRODUCER_AS_WORKER", allowProducerAsWorker),
		envKV("PRODUCER_AUTH_PROVIDER", producerAuthProvider),
		envKV("PRODUCER_AUTH_CONFIG", producerAuthConfig),
		envKV("WORKER_AUTH_PROVIDER", workerAuthProvider),
		envKV("WORKER_AUTH_CONFIG", workerAuthConfig),
		envKV("TRACING_ENABLED", "false"),
		envKV("TRACING_OTLP_ENDPOINT", ""),
		envKV("TRACING_OTLP_INSECURE", "false"),
		envKV("TRACING_SAMPLE_RATIO", "1.0"),
	}
	return strings.Join(lines, "\n") + "\n"
}

func renderHelmInstallValues(opts installOptions, profile installProfile) (string, []string) {
	var warnings []string
	imageRepo, imageTag := splitImageRef(opts.Image)
	redisAddr := strings.TrimSpace(opts.RedisAddr)
	embeddedKVRocks := profile.EmbeddedKVRocks && redisAddr == ""
	if !embeddedKVRocks && redisAddr == "" {
		redisAddr = "CHANGE_ME_KVROCKS:6666"
		warnings = append(warnings, profile.Name+" profile expects an external KVRocks/Redis address; set --redis-addr before executing.")
	}
	if embeddedKVRocks {
		redisAddr = "127.0.0.1:6379"
	}

	envMode := "prod"
	logLevel := "info"
	allowProducerAsWorker := false
	extraEnv := "extraEnv: []\n"
	if profile.DevAuth {
		envMode = "dev"
		logLevel = "debug"
		allowProducerAsWorker = true
		extraEnv = `extraEnv:
  - name: PRODUCER_AUTH_PROVIDER
    value: static
  - name: PRODUCER_AUTH_CONFIG
    value: '{"token":"dev-token","subject":"producer-dev","email":"dev@codeq.local","raw":{"role":"ADMIN"}}'
  - name: WORKER_AUTH_PROVIDER
    value: static
  - name: WORKER_AUTH_CONFIG
    value: '{"token":"dev-token","subject":"worker-dev","scopes":["codeq:claim","codeq:heartbeat","codeq:abandon","codeq:nack","codeq:result","codeq:subscribe"],"eventTypes":["*"]}'
`
	}

	ingressBlock := "ingress:\n  enabled: false\n"
	if strings.TrimSpace(opts.Domain) != "" {
		ingressBlock = fmt.Sprintf(`ingress:
  enabled: true
  hosts:
    - host: %q
      paths:
        - path: /v1/codeq
          pathType: Prefix
`, opts.Domain)
	}

	return fmt.Sprintf(`image:
  repository: %q
  tag: %q
  pullPolicy: IfNotPresent

replicaCount: %d

config:
  env: %q
  logLevel: %q
  logFormat: json
  redisAddr: %q
  identityServiceUrl: %q
  workerJwksUrl: %q
  workerIssuer: %q
  workerAudience: %q
  allowProducerAsWorker: %t
  defaultLeaseSeconds: 300
  requeueInspectLimit: %d
  maxAttemptsDefault: 5
  backoffPolicy: exp_full_jitter
  backoffBaseSeconds: 5
  backoffMaxSeconds: 900

secrets:
  enabled: true
  webhookHmacSecret: %q

autoscaling:
  enabled: %t
  minReplicas: %d
  maxReplicas: %d
  targetCPUUtilizationPercentage: %d

resources:
  requests:
    cpu: %q
    memory: %q
  limits:
    cpu: %q
    memory: %q

kvrocks:
  enabled: %t
  persistence:
    enabled: true
    size: %q
  resources:
    requests:
      cpu: %q
      memory: %q
    limits:
      cpu: %q
      memory: %q

persistence:
  artifacts:
    enabled: %t
    size: %q

%s
%s`, imageRepo, imageTag, profile.Replicas, envMode, logLevel, redisAddr,
		opts.IdentityServiceURL, opts.WorkerJwksURL, opts.WorkerIssuer, emptyOr(opts.WorkerAudience, "codeq-worker"),
		allowProducerAsWorker, profile.RequeueInspectLimit, opts.WebhookHmacSecret,
		profile.Name != "dev", profile.MinReplicas, profile.MaxReplicas, profile.TargetCPU,
		profile.CPURequest, profile.MemoryRequest, profile.CPULimit, profile.MemoryLimit,
		embeddedKVRocks, profile.KVRocksSize, emptyOr(profile.KVRocksCPURequest, "250m"),
		emptyOr(profile.KVRocksMemoryRequest, "512Mi"), emptyOr(profile.KVRocksCPULimit, "1"),
		emptyOr(profile.KVRocksMemoryLimit, "1Gi"), profile.ArtifactsEnabled, emptyOr(profile.ArtifactsSize, "1Gi"),
		ingressBlock, extraEnv), warnings
}

func validateExecutableInstall(opts installOptions, profile installProfile) error {
	if opts.Target == "kubernetes" && isLocalChart(opts.Chart) && !fileExists(filepath.Join(opts.Chart, "Chart.yaml")) {
		return fmt.Errorf("helm chart not found at %s", opts.Chart)
	}
	if profile.DevAuth {
		return nil
	}
	missing := []string{}
	if strings.Contains(opts.IdentityServiceURL, "example.com") || strings.TrimSpace(opts.IdentityServiceURL) == "" {
		missing = append(missing, "--identity-service-url")
	}
	if strings.Contains(opts.WorkerJwksURL, "example.com") || strings.TrimSpace(opts.WorkerJwksURL) == "" {
		missing = append(missing, "--worker-jwks-url")
	}
	if strings.Contains(opts.WorkerIssuer, "example.com") || strings.TrimSpace(opts.WorkerIssuer) == "" {
		missing = append(missing, "--worker-issuer")
	}
	if opts.Target == "kubernetes" && !profile.EmbeddedKVRocks && strings.TrimSpace(opts.RedisAddr) == "" {
		missing = append(missing, "--redis-addr")
	}
	if len(missing) > 0 {
		return fmt.Errorf("refusing --execute with placeholder production settings; provide %s", strings.Join(missing, ", "))
	}
	return nil
}

func runInstallCommand(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("empty install command")
	}
	// #nosec G204 -- args is an internally generated docker/helm command vector, never a shell string.
	proc := exec.CommandContext(ctx, args[0], args[1:]...)
	proc.Stdout = os.Stdout
	proc.Stderr = os.Stderr
	proc.Stdin = os.Stdin
	return proc.Run()
}

func printInstallBundle(bundle installBundle, ui *ui) {
	fmt.Printf("%s Installation bundle written to %s\n", ui.ok("[OK]"), bundle.OutputDir)
	for _, file := range bundle.Files {
		fmt.Printf("  %s\n", file)
	}
	for _, warning := range bundle.Warnings {
		fmt.Printf("%s %s\n", ui.warn("[WARN]"), warning)
	}
	fmt.Println()
	if len(bundle.BuildCommand) > 0 {
		fmt.Println("Build command:")
		fmt.Printf("  %s\n", shellJoin(bundle.BuildCommand))
		fmt.Println()
	}
	fmt.Println("Next command:")
	fmt.Printf("  %s\n", shellJoin(bundle.Command))
}

func workerLoop(ctx context.Context, c *client, events []string, leaseSec int, waitSec int, ackMode string, nackDelay int, ui *ui) {
	payload := map[string]any{
		"commands":     events,
		"leaseSeconds": leaseSec,
		"waitSeconds":  waitSec,
	}
	var lastErr time.Time
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		status, resp, err := c.request("POST", "/v1/codeq/tasks/claim", c.workerToken, payload)
		if err != nil {
			if time.Since(lastErr) > 3*time.Second {
				fmt.Printf("%s claim error: %s\n", ui.warn("[WARN]"), err.Error())
				lastErr = time.Now()
			}
			time.Sleep(1 * time.Second)
			continue
		}
		if status == http.StatusNoContent {
			continue
		}
		if status != http.StatusOK {
			if time.Since(lastErr) > 3*time.Second {
				fmt.Printf("%s claim denied (%d): %s\n", ui.warn("[WARN]"), status, strings.TrimSpace(string(resp)))
				lastErr = time.Now()
			}
			time.Sleep(1 * time.Second)
			continue
		}
		var task taskResp
		if err := json.Unmarshal(resp, &task); err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		fmt.Printf("%s Task claimed: %s (%s)\n", ui.ok("[OK]"), task.ID, task.Command)
		switch ackMode {
		case "complete":
			_, _, _ = c.request("POST", "/v1/codeq/tasks/"+url.PathEscape(task.ID)+"/result", c.workerToken, map[string]any{
				"status": "COMPLETED",
				"result": map[string]any{
					"handledBy": "codeq",
					"ts":        time.Now().UTC().Format(time.RFC3339),
				},
			})
		case "nack":
			_, _, _ = c.request("POST", "/v1/codeq/tasks/"+url.PathEscape(task.ID)+"/nack", c.workerToken, map[string]any{
				"delaySeconds": nackDelay,
				"reason":       "codeq",
			})
		case "abandon":
			_, _, _ = c.request("POST", "/v1/codeq/tasks/"+url.PathEscape(task.ID)+"/abandon", c.workerToken, nil)
		case "none":
		}
	}
}

func newClient(baseURL, producerToken, workerToken string, admin bool) *client {
	return &client{
		baseURL:       strings.TrimRight(baseURL, "/"),
		producerToken: producerToken,
		workerToken:   workerToken,
		admin:         admin,
		httpClient:    &http.Client{Timeout: 30 * time.Second},
	}
}

func getenv(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func getenvBool(k string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(k)))
	if v == "" {
		return def
	}
	return v == "1" || v == "true" || v == "yes"
}

func isLocalURL(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1"
}

func producerAuthToken(producer, worker string) string {
	if strings.TrimSpace(producer) != "" {
		return producer
	}
	if strings.TrimSpace(worker) != "" {
		return worker
	}
	return ""
}

func workerAuthToken(worker, producer string) string {
	if strings.TrimSpace(worker) != "" {
		return worker
	}
	if strings.TrimSpace(producer) != "" {
		return producer
	}
	return ""
}

func splitEvents(s string) []string {
	var out []string
	for _, e := range strings.Split(s, ",") {
		e = strings.TrimSpace(e)
		if e != "" {
			out = append(out, e)
		}
	}
	return out
}

func helpTemplate(ui *ui) string {
	title := ui.title("codeq")
	return fmt.Sprintf(`%s — CLI for codeQ

Usage:
  {{.UseLine}}

Commands:
{{range .Commands}}{{if (or .IsAvailableCommand .IsAdditionalHelpTopicCommand)}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}

Flags:
  {{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}

Global Flags:
  {{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}

Config:
  %s

Examples:
  codeq init
  codeq auth login --email you@company.com
  codeq task create --event render_video --priority 10 --payload '{"jobId":500}'
  codeq worker start --events render_video --concurrency 5
  codeq queue inspect render_video
  codeq install --target kubernetes --size small

`, title, configPath())
}

func configPath() string {
	if v := strings.TrimSpace(os.Getenv("CODEQ_CONFIG_DIR")); v != "" {
		return filepath.Join(v, "config.yaml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./config.yaml"
	}
	return filepath.Join(home, ".codeq", "config.yaml")
}

func defaultLoginConfig(iamBaseURL, apiKey string) loginConfig {
	base := strings.TrimRight(iamBaseURL, "/")
	if base == "" {
		base = defaultIAMBaseURL
	}
	return loginConfig{
		URLTemplate:  base + "/signInWithPassword?key={{apiKey}}",
		Method:       "POST",
		ContentType:  "application/json",
		TokenPath:    "idToken",
		BodyTemplate: `{"email":"{{email}}","password":"{{password}}"}`,
		Headers:      map[string]string{},
	}
}

func defaultExchangeConfig(iamBaseURL, apiKey string) exchangeConfig {
	base := strings.TrimRight(iamBaseURL, "/")
	if base == "" {
		base = defaultIAMBaseURL
	}
	return exchangeConfig{
		URLTemplate: base + "/token/exchange?key={{apiKey}}",
		Method:      "POST",
		ContentType: "application/json",
		TokenPath:   "accessToken",
		Audience:    "codeq-producer",
		Headers:     map[string]string{},
	}
}

func iamLoginGeneric(cfg loginConfig, iamBaseURL, apiKey, email, password string) (string, error) {
	if strings.TrimSpace(cfg.URLTemplate) == "" {
		cfg = defaultLoginConfig(iamBaseURL, apiKey)
	}
	if cfg.Method == "" {
		cfg.Method = "POST"
	}
	if cfg.ContentType == "" {
		cfg.ContentType = "application/json"
	}
	if cfg.TokenPath == "" {
		cfg.TokenPath = "idToken"
	}

	vars := map[string]string{
		"email":      email,
		"password":   password,
		"apiKey":     apiKey,
		"iamBaseUrl": strings.TrimRight(iamBaseURL, "/"),
	}
	loginURL, err := renderTemplate(cfg.URLTemplate, vars)
	if err != nil {
		return "", err
	}
	bodyStr, err := renderTemplate(cfg.BodyTemplate, vars)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequest(cfg.Method, loginURL, bytes.NewReader([]byte(bodyStr)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", cfg.ContentType)
	for k, v := range cfg.Headers {
		if strings.TrimSpace(k) != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("login failed (%d): %s", resp.StatusCode, string(out))
	}
	raw, _ := io.ReadAll(resp.Body)
	token, err := extractToken(raw, cfg.TokenPath)
	if err != nil {
		return "", err
	}
	return token, nil
}

func iamExchange(cfg exchangeConfig, iamBaseURL, apiKey, idToken string) (string, error) {
	if strings.TrimSpace(cfg.URLTemplate) == "" {
		cfg = defaultExchangeConfig(iamBaseURL, apiKey)
	}
	if cfg.Method == "" {
		cfg.Method = "POST"
	}
	if cfg.ContentType == "" {
		cfg.ContentType = "application/json"
	}
	if cfg.TokenPath == "" {
		cfg.TokenPath = "accessToken"
	}

	vars := map[string]string{
		"apiKey":     apiKey,
		"iamBaseUrl": strings.TrimRight(iamBaseURL, "/"),
	}
	exchangeURL, err := renderTemplate(cfg.URLTemplate, vars)
	if err != nil {
		return "", err
	}

	body := map[string]any{
		"idToken": idToken,
	}
	if strings.TrimSpace(cfg.Audience) != "" {
		body["audience"] = strings.TrimSpace(cfg.Audience)
	}
	if strings.TrimSpace(cfg.TenantID) != "" {
		body["tenantId"] = strings.TrimSpace(cfg.TenantID)
	}
	if len(cfg.Scopes) > 0 {
		var scopes []string
		for _, s := range cfg.Scopes {
			if v := strings.TrimSpace(s); v != "" {
				scopes = append(scopes, v)
			}
		}
		if len(scopes) > 0 {
			body["scopes"] = scopes
		}
	}
	b, _ := json.Marshal(body)

	req, err := http.NewRequest(cfg.Method, exchangeURL, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", cfg.ContentType)
	for k, v := range cfg.Headers {
		if strings.TrimSpace(k) != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("token exchange failed (%d): %s", resp.StatusCode, string(out))
	}
	raw, _ := io.ReadAll(resp.Body)
	token, err := extractToken(raw, cfg.TokenPath)
	if err != nil {
		return "", err
	}
	return token, nil
}

func renderTemplate(tpl string, vars map[string]string) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		return "", errors.New("payload template is empty")
	}
	funcs := template.FuncMap{}
	for k, v := range vars {
		val := v
		funcs[k] = func() string { return val }
	}
	t, err := template.New("tpl").Funcs(funcs).Option("missingkey=error").Parse(tpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, vars); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func extractToken(body []byte, path string) (string, error) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("invalid JSON response")
	}
	curr := v
	for _, p := range strings.Split(path, ".") {
		if p == "" {
			continue
		}
		m, ok := curr.(map[string]any)
		if !ok {
			return "", fmt.Errorf("token path not found")
		}
		curr, ok = m[p]
		if !ok {
			return "", fmt.Errorf("token path not found")
		}
	}
	if s, ok := curr.(string); ok && strings.TrimSpace(s) != "" {
		return s, nil
	}
	return "", fmt.Errorf("token not found at path")
}

func iamLogin(baseURL, apiKey, email, password string) (string, error) {
	u := strings.TrimRight(baseURL, "/") + "/signInWithPassword?key=" + url.QueryEscape(apiKey)
	body := map[string]string{"email": email, "password": password}
	b, _ := json.Marshal(body)
	req, err := http.NewRequest(http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		out, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("login failed (%d): %s", resp.StatusCode, string(out))
	}
	var out struct {
		IDToken string `json:"idToken"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.IDToken == "" {
		return "", errors.New("login returned empty token")
	}
	return out.IDToken, nil
}

func iamLoginURL(loginURL, email, password string) (string, error) {
	return iamLoginGeneric(loginConfig{
		URLTemplate:  loginURL,
		Method:       "POST",
		ContentType:  "application/json",
		TokenPath:    "idToken",
		BodyTemplate: `{"email":"{{email}}","password":"{{password}}"}`,
	}, "", "", email, password)
}

func promptSecret(label string) (string, error) {
	fmt.Printf("%s: ", label)
	b, err := termReadPassword()
	fmt.Println()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func termReadPassword() ([]byte, error) {
	fd := int(os.Stdin.Fd())
	if !isTerminal(fd) {
		reader := bufio.NewReader(os.Stdin)
		line, err := reader.ReadString('\n')
		return []byte(strings.TrimSpace(line)), err
	}
	return term.ReadPassword(fd)
}

func isTerminal(fd int) bool {
	return term.IsTerminal(fd)
}

func loadConfig() (cliConfig, string, error) {
	path := configPath()
	var cfg cliConfig
	// #nosec G304 -- path is the operator's same-user CLI configuration path.
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cliConfig{Profiles: map[string]profile{}}, path, nil
		}
		return cfg, path, err
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, path, err
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]profile{}
	}
	return cfg, path, nil
}

func saveConfig(cfg cliConfig, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func resolveProfileName(flag string, cfg cliConfig) string {
	if strings.TrimSpace(flag) != "" {
		return strings.TrimSpace(flag)
	}
	if v := strings.TrimSpace(os.Getenv("CODEQ_PROFILE")); v != "" {
		return v
	}
	if cfg.CurrentProfile != "" {
		return cfg.CurrentProfile
	}
	return "default"
}

func profileFromEmail(email string) string {
	email = strings.TrimSpace(strings.ToLower(email))
	email = strings.ReplaceAll(email, "@", "_")
	email = strings.ReplaceAll(email, ".", "_")
	if email == "" {
		return "default"
	}
	return email
}

func prompt(r *bufio.Reader, label, def string) string {
	if def != "" {
		fmt.Printf("%s [%s]: ", label, def)
	} else {
		fmt.Printf("%s: ", label)
	}
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func promptChoice(r *bufio.Reader, label, def string, allowed []string) string {
	allowedSet := map[string]struct{}{}
	for _, v := range allowed {
		allowedSet[strings.ToLower(v)] = struct{}{}
	}
	for {
		v := strings.ToLower(strings.TrimSpace(prompt(r, label, def)))
		if _, ok := allowedSet[v]; ok {
			return v
		}
		fmt.Printf("Choose one of: %s\n", strings.Join(allowed, ", "))
	}
}

func promptYesNo(r *bufio.Reader, label string, def bool) bool {
	defLabel := "n"
	if def {
		defLabel = "y"
	}
	for {
		v := strings.ToLower(strings.TrimSpace(prompt(r, label+" (y/n)", defLabel)))
		switch v {
		case "y", "yes", "s", "sim", "true", "1":
			return true
		case "n", "no", "nao", "false", "0":
			return false
		}
		fmt.Println("Answer y or n.")
	}
}

func randomHex(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func envKV(key, value string) string {
	value = strings.ReplaceAll(value, "\n", "")
	value = strings.ReplaceAll(value, "\r", "")
	return key + "=" + value
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isLocalChart(chart string) bool {
	chart = strings.TrimSpace(chart)
	if chart == "" {
		return false
	}
	return strings.HasPrefix(chart, ".") || strings.HasPrefix(chart, "/")
}

func shellJoin(args []string) string {
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuote(arg))
	}
	return strings.Join(quoted, " ")
}

func shellQuote(arg string) string {
	if arg == "" {
		return "''"
	}
	if !strings.ContainsAny(arg, " \t\n'\"\\$`!*?[]{}()<>|&;") {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
}

func splitImageRef(image string) (string, string) {
	image = strings.TrimSpace(image)
	if image == "" {
		return "ghcr.io/osvaldoandrade/codeq-service", "0.1.0"
	}
	if at := strings.LastIndex(image, "@"); at > -1 {
		return image[:at], image[at+1:]
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon > lastSlash {
		return image[:lastColon], image[lastColon+1:]
	}
	return image, "latest"
}

func maskToken(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return "<unset>"
	}
	if len(v) <= 8 {
		return "****"
	}
	return v[:4] + "..." + v[len(v)-4:]
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func emptyOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
