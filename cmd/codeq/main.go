package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
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
	Login loginConfig `yaml:"login"`
}

type loginConfig struct {
	URLTemplate  string            `yaml:"urlTemplate"`
	Method       string            `yaml:"method"`
	Headers      map[string]string `yaml:"headers"`
	BodyTemplate string            `yaml:"bodyTemplate"`
	ContentType  string            `yaml:"contentType"`
	TokenPath    string            `yaml:"tokenPath"`
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
		if !flags.Changed("producer-token") {
			if v := strings.TrimSpace(os.Getenv("CODEQ_PRODUCER_TOKEN")); v != "" {
				producerToken = v
			} else if prof.Token != "" {
				producerToken = prof.Token
			} else if prof.ProducerToken != "" {
				producerToken = prof.ProducerToken
			}
		}
		if !flags.Changed("worker-token") {
			if v := strings.TrimSpace(os.Getenv("CODEQ_WORKER_TOKEN")); v != "" {
				workerToken = v
			} else if prof.Token != "" {
				workerToken = prof.Token
			} else if prof.WorkerToken != "" {
				workerToken = prof.WorkerToken
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
		loginEmail       string
		loginPassword    string
		loginURL         string
		loginMethod      string
		loginCT          string
		loginPayload     string
		loginPayloadFile string
		loginTokenPath   string
		saveLoginConfig  bool
		headerKVs        []string
		noPrompt         bool
	)
	login := &cobra.Command{
		Use:   "login",
		Short: "Login via IAM and store token",
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

			token, err := iamLoginGeneric(loginCfg, prof.IAMBaseURL, prof.IAMAPIKey, email, password)
			if err != nil {
				return err
			}
			prof.Token = token
			prof.ProducerToken = token
			prof.WorkerToken = token

			if cfg.Profiles == nil {
				cfg.Profiles = map[string]profile{}
			}
			if saveLoginConfig {
				prof.Auth.Login = loginCfg
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
	login.Flags().BoolVar(&saveLoginConfig, "save", true, "Save login config for this profile")
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
			fmt.Printf("%s Profile: %s\n", ui.title("codeq"), active)
			fmt.Printf("%s Base URL: %s\n", ui.info("•"), emptyOr(prof.BaseURL, "<unset>"))
			fmt.Printf("%s IAM URL:  %s\n", ui.info("•"), emptyOr(prof.IAMBaseURL, "<unset>"))
			fmt.Printf("%s API Key:  %s\n", ui.info("•"), maskToken(prof.IAMAPIKey))
			fmt.Printf("%s Login URL: %s\n", ui.info("•"), emptyOr(prof.Auth.Login.URLTemplate, "<unset>"))
			fmt.Printf("%s Token Path: %s\n", ui.info("•"), emptyOr(prof.Auth.Login.TokenPath, "<unset>"))
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
			token := workerAuthToken(*workerToken, *producerToken)
			if strings.TrimSpace(token) == "" {
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
			fmt.Printf("%s Worker connected. Listening...\n", ui.info("[INFO]"))

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

func workerLoop(ctx context.Context, c *client, events []string, leaseSec int, waitSec int, ackMode string, nackDelay int, ui *ui) {
	payload := map[string]any{
		"commands":     events,
		"leaseSeconds": leaseSec,
		"waitSeconds":  waitSec,
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		status, resp, err := c.request("POST", "/v1/codeq/tasks/claim", c.workerToken, payload)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		if status == http.StatusNoContent {
			continue
		}
		if status != http.StatusOK {
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
