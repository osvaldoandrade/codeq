package app

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/osvaldoandrade/codeq/internal/producer/producerpb"
	"github.com/osvaldoandrade/codeq/internal/worker/workerpb"
	_ "github.com/osvaldoandrade/codeq/pkg/auth/jwks"   // Register JWKS provider
	_ "github.com/osvaldoandrade/codeq/pkg/auth/multi"  // Register multi provider
	_ "github.com/osvaldoandrade/codeq/pkg/auth/static" // Register static provider
	"github.com/osvaldoandrade/codeq/pkg/config"
)

// The binding-scope e2e suite runs the production router with the same
// validator shape the Conveste installation uses: a static platform token and
// a Tikti JWKS provider behind "multi" for producers and workers.

const (
	bindingIssuer      = "https://tikti.binding.test"
	bindingKid         = "binding-kid"
	bindingTopic       = "cflow-executar"
	staticProducerTok  = "static-producer-token"
	staticWorkerTok    = "static-worker-token"
	bindingWorkerScope = "codeq:abandon codeq:claim codeq:heartbeat codeq:nack codeq:result"
)

var okResult = map[string]any{"ok": true}

const (
	bindingEnv       = "test"
	producerAudience = "codeq-producer"
	policyPublish    = "Publish"
	policySubscribe  = "Subscribe"
	claimAudience    = "aud"
	claimSubject     = "sub"
	claimExpiry      = "exp"
	claimIssuer      = "iss"
	claimScope       = "scope"
	claimJTI         = "jti"
	claimIssuedAt    = "iat"
	claimEventTypes  = "eventTypes"
	keyPayload       = "payload"
	keyResult        = "result"
	keyStatus        = "status"
	keyCommand       = "command"
	keyCommands      = "commands"
	keyWebhook       = "webhook"
	keyTasks         = "tasks"
	tenantConveste   = "conveste"
	statusCompleted  = "COMPLETED"
	pathTasks        = "/v1/codeq/tasks"
	pathClaim        = pathTasks + "/claim"
)

type bindingSuite struct {
	t          *testing.T
	key        *rsa.PrivateKey
	base       string
	workerAddr string
	prodAddr   string
}

func newBindingSuite(t *testing.T, fsync ...bool) *bindingSuite {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa: %v", err)
	}
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		_, _ = fmt.Fprintf(w, `{"keys":[{"kty":"RSA","kid":%q,"n":%q,"e":"AQAB"}]}`, bindingKid, n)
	}))
	t.Cleanup(jwks.Close)
	s := &bindingSuite{t: t, key: key, workerAddr: freeAddr(t), prodAddr: freeAddr(t)}
	cfg := s.config(jwks.URL)
	if len(fsync) > 0 && fsync[0] {
		var persistence map[string]any
		_ = json.Unmarshal(cfg.PersistenceConfig, &persistence)
		persistence["fsyncOnCommit"] = true
		cfg.PersistenceConfig, _ = json.Marshal(persistence)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("config: %v", err)
	}
	application, err := NewApplication(cfg)
	if err != nil {
		t.Fatalf("app: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = application.TracingShutdown(ctx)
	})
	SetupMappings(application)
	server := httptest.NewServer(application.Engine)
	t.Cleanup(server.Close)
	s.base = server.URL
	time.Sleep(150 * time.Millisecond) // let the gRPC listeners accept
	return s
}

func freeAddr(t *testing.T) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := lis.Addr().String()
	_ = lis.Close()
	return addr
}

// providerTemplate mirrors the Conveste multi provider: one static token
// first, then the Tikti JWKS provider for the given audience.
const providerTemplate = `{"providers":[{"type":"static","config":%s},
 {"type":"jwks","config":{"jwksUrl":%q,"issuer":%q,"audience":%q,"clockSkew":60000000000,"httpTimeout":5000000000}}]}`

func (s *bindingSuite) config(jwksURL string) *config.Config {
	staticProducer := fmt.Sprintf(`{"token":%q,"subject":"codecloud-producer","email":"platform@codecloud.invalid",
 "raw":{"role":"ADMIN","tenantId":"local-tenant"}}`, staticProducerTok)
	staticWorker := fmt.Sprintf(`{"token":%q,"subject":"codecloud-worker","eventTypes":["*"],
 "scopes":["codeq:claim","codeq:heartbeat","codeq:abandon","codeq:nack","codeq:result","codeq:subscribe"],
 "raw":{"tenantId":"local-tenant"}}`, staticWorkerTok)
	producer := json.RawMessage(fmt.Sprintf(providerTemplate, staticProducer, jwksURL, bindingIssuer, producerAudience))
	worker := json.RawMessage(fmt.Sprintf(providerTemplate, staticWorker, jwksURL, bindingIssuer, topicTestWorkerAudience))
	return &config.Config{
		Timezone: topicTestTimezone, LogLevel: topicTestLogLevel, LogFormat: topicTestLogFormat, Env: bindingEnv,
		DefaultLeaseSeconds: 60, RequeueInspectLimit: 50, LocalArtifactsDir: s.t.TempDir(),
		MaxAttemptsDefault: 5, BackoffPolicy: topicTestBackoffPolicy, BackoffBaseSeconds: 1, BackoffMaxSeconds: 3,
		WebhookHmacSecret: topicTestSecret, WorkerAudience: topicTestWorkerAudience, AllowedClockSkewSeconds: 60,
		// Required by Validate in non-dev; inert because explicit providers are set.
		WorkerJwksURL: jwksURL, WorkerIssuer: bindingIssuer, IdentityJwksURL: jwksURL,
		SubscriptionMinIntervalSeconds: 5, SubscriptionCleanupIntervalSeconds: 60,
		ResultWebhookMaxAttempts: 3, ResultWebhookBaseBackoffSeconds: 1, ResultWebhookMaxBackoffSeconds: 2,
		ProducerAuthProvider: "multi", ProducerAuthConfig: producer,
		WorkerAuthProvider: "multi", WorkerAuthConfig: worker,
		PersistenceProvider: topicTestStorageProvider,
		PersistenceConfig:   json.RawMessage(fmt.Sprintf(`{"path":%q}`, s.t.TempDir())),
		RedisAddr:           topicTestRedisAddr,
		WorkerStreamAddr:    s.workerAddr,
		ProducerStreamAddr:  s.prodAddr,
	}
}

func (s *bindingSuite) sign(claims map[string]any) string {
	s.t.Helper()
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT","kid":"` + bindingKid + `"}`))
	input := header + "." + enc(claims)
	digest := sha256.Sum256([]byte(input))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		s.t.Fatalf("sign: %v", err)
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(sig)
}

// bindingToken mints an ADR-0022 C3 token. mutate may edit the claims.
func (s *bindingSuite) bindingToken(policy, tenant, topic, pod string, mutate func(map[string]any)) string {
	aud, scope := producerAudience, "codeq:publish"
	if policy == policySubscribe {
		aud, scope = topicTestWorkerAudience, bindingWorkerScope
	}
	now := time.Now().Unix()
	claims := map[string]any{
		claimIssuer: bindingIssuer, claimAudience: aud, "tid": tenant, claimScope: scope,
		claimSubject:    "codefoundry:workload:conveste-hostgator:workload-" + tenant + ":app:" + pod,
		claimEventTypes: []string{topic}, "cluster_ref": "conveste-hostgator",
		claimIssuedAt: now - 5, claimExpiry: now + 295, claimJTI: pod + "-" + policy,
		"codeq_binding": map[string]any{"uid": "binding-" + pod, "generation": 2, "policy": policy, "topicId": tenant + "." + topic},
	}
	if mutate != nil {
		mutate(claims)
	}
	return s.sign(claims)
}

func (s *bindingSuite) adminToken(tenant string) string {
	now := time.Now().Unix()
	return s.sign(map[string]any{
		claimIssuer: bindingIssuer, claimAudience: producerAudience, claimSubject: "system:serviceaccount:platform:" + tenant,
		"tid": tenant, claimScope: "codeq:admin", claimIssuedAt: now - 5, claimExpiry: now + 295,
	})
}

type apiResult struct {
	status int
	body   map[string]any
	raw    string
}

func (s *bindingSuite) call(method, path, token string, body any) apiResult {
	s.t.Helper()
	var reader io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.base+path, reader)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := apiResult{status: resp.StatusCode, raw: strings.TrimSpace(string(raw))}
	_ = json.Unmarshal(raw, &out.body)
	return out
}

func (s *bindingSuite) expect(r apiResult, status int, errCode string, label string) {
	s.t.Helper()
	if r.status != status {
		s.t.Fatalf("%s: status %d, want %d (body %s)", label, r.status, status, r.raw)
	}
	if errCode != "" && r.body["error"] != errCode {
		s.t.Fatalf("%s: error %v, want %q", label, r.body["error"], errCode)
	}
}

func (s *bindingSuite) createTask(token, command string) string {
	s.t.Helper()
	r := s.call(http.MethodPost, pathTasks, token, map[string]any{keyCommand: command, keyPayload: map[string]any{"k": "v"}})
	s.expect(r, http.StatusAccepted, "", "create "+command)
	id, _ := r.body["id"].(string)
	if id == "" {
		s.t.Fatalf("create returned no id: %s", r.raw)
	}
	return id
}

func (s *bindingSuite) readyDepth(adminToken, command string) float64 {
	s.t.Helper()
	r := s.call(http.MethodGet, "/v1/codeq/admin/queues/"+command, adminToken, nil)
	s.expect(r, http.StatusOK, "", "queue stats")
	ready, _ := r.body["ready"].(float64)
	return ready
}

func TestBindingScopeE2E(t *testing.T) {
	s := newBindingSuite(t)
	publish := s.bindingToken(policyPublish, tenantConveste, bindingTopic, "pod-1", nil)
	subscribe := s.bindingToken(policySubscribe, tenantConveste, bindingTopic, "pod-1", nil)
	t.Run("publish", func(t *testing.T) { s.t = t; s.publishRules(publish) })
	t.Run("batch", func(t *testing.T) { s.t = t; s.batchAllOrNothing(publish) })
	t.Run("read isolation", func(t *testing.T) { s.t = t; s.readIsolation(publish, subscribe) })
	t.Run("worker ownership", func(t *testing.T) { s.t = t; s.workerOwnership(publish, subscribe) })
	t.Run("route allow-list", func(t *testing.T) { s.t = t; s.routeAllowList(publish, subscribe) })
	t.Run("invalid binding tokens", func(t *testing.T) { s.t = t; s.invalidTokens() })
	t.Run("legacy unchanged", func(t *testing.T) { s.t = t; s.legacyUnchanged() })
	t.Run("grpc streams", func(t *testing.T) { s.t = t; s.grpcStreams(publish, subscribe) })
}

func (s *bindingSuite) publishRules(publish string) {
	id := s.createTask(publish, bindingTopic)
	task := s.call(http.MethodGet, "/v1/codeq/tasks/"+id, publish, nil)
	s.expect(task, http.StatusOK, "", "own task")
	if task.body["tenantId"] != tenantConveste || task.body["command"] != bindingTopic {
		s.t.Fatalf("task not bound to tid/command: %s", task.raw)
	}
	r := s.call(http.MethodPost, pathTasks, publish, map[string]any{keyCommand: "other-topic", keyPayload: 1})
	s.expect(r, http.StatusForbidden, "event_type_not_allowed", "other command")
	r = s.call(http.MethodPost, pathTasks, publish, map[string]any{keyCommand: "conveste." + bindingTopic, keyPayload: 1})
	s.expect(r, http.StatusForbidden, "event_type_not_allowed", "topicId as command")
	r = s.call(http.MethodPost, pathTasks, publish, map[string]any{keyCommand: bindingTopic, keyPayload: 1, keyWebhook: "http://169.254.169.254/latest"})
	s.expect(r, http.StatusForbidden, "webhook_not_allowed", "webhook")
}

func (s *bindingSuite) batchAllOrNothing(publish string) {
	admin := s.adminToken(tenantConveste)
	before := s.readyDepth(admin, bindingTopic)
	item := func(command, webhook string) map[string]any {
		return map[string]any{keyCommand: command, keyPayload: 1, keyWebhook: webhook}
	}
	r := s.call(http.MethodPost, "/v1/codeq/tasks/batch", publish, map[string]any{keyTasks: []any{item(bindingTopic, ""), item(bindingTopic, "https://example.com/hook")}})
	s.expect(r, http.StatusForbidden, "webhook_not_allowed", "batch webhook")
	if r.body["index"] != float64(1) {
		s.t.Fatalf("batch index = %v, want 1", r.body["index"])
	}
	r = s.call(http.MethodPost, "/v1/codeq/tasks/batch", publish, map[string]any{keyTasks: []any{item(bindingTopic, ""), item(bindingTopic, ""), item("other", "")}})
	s.expect(r, http.StatusForbidden, "event_type_not_allowed", "batch command")
	if r.body["index"] != float64(2) {
		s.t.Fatalf("batch index = %v, want 2", r.body["index"])
	}
	if after := s.readyDepth(admin, bindingTopic); after != before {
		s.t.Fatalf("refused batch enqueued tasks: ready %v -> %v", before, after)
	}
	r = s.call(http.MethodPost, "/v1/codeq/tasks/batch", publish, map[string]any{keyTasks: []any{
		item(bindingTopic, ""), map[string]any{keyCommand: bindingTopic, keyPayload: 1, "runAt": "not-a-time"},
	}})
	s.expect(r, http.StatusOK, "", "batch per-item validation")
	results, _ := r.body["results"].([]any)
	if len(results) != 2 || results[1].(map[string]any)["error"] == nil {
		s.t.Fatalf("per-item validation changed: %s", r.raw)
	}
	if after := s.readyDepth(admin, bindingTopic); after != before+1 {
		s.t.Fatalf("authorized batch enqueued %v, want 1", after-before)
	}
}

func (s *bindingSuite) readIsolation(publish, subscribe string) {
	id := s.createTask(publish, bindingTopic)
	localID := s.createTask(staticProducerTok, bindingTopic)
	allowed := map[string]string{
		"binding publish": publish, "binding subscribe": subscribe, "conveste admin": s.adminToken(tenantConveste),
	}
	denied := map[string]string{
		"static producer (local-tenant)": staticProducerTok,
		"static worker (local-tenant)":   staticWorkerTok,
		"platform admin (local-tenant)":  s.adminToken("local-tenant"),
		"cross-tenant binding":           s.bindingToken(policyPublish, "other", bindingTopic, "pod-9", nil),
		"same tenant other topic":        s.bindingToken(policyPublish, tenantConveste, "other-topic", "pod-8", nil),
	}
	for name, token := range allowed {
		s.expect(s.call(http.MethodGet, "/v1/codeq/tasks/"+id, token, nil), http.StatusOK, "", name+" GET task")
		s.expect(s.call(http.MethodGet, "/v1/codeq/tasks/"+id+"/result", token, nil), http.StatusNotFound, "result not found", name+" GET result")
	}
	missingTask := s.call(http.MethodGet, "/v1/codeq/tasks/does-not-exist", publish, nil)
	missingResult := s.call(http.MethodGet, "/v1/codeq/tasks/does-not-exist/result?waitSeconds=5", publish, nil)
	for name, token := range denied {
		got := s.call(http.MethodGet, "/v1/codeq/tasks/"+id, token, nil)
		if got.status != http.StatusNotFound || got.raw != missingTask.raw {
			s.t.Fatalf("%s GET task: %d %s, want the missing-task response %s", name, got.status, got.raw, missingTask.raw)
		}
		start := time.Now()
		got = s.call(http.MethodGet, "/v1/codeq/tasks/"+id+"/result?waitSeconds=5", token, nil)
		if got.status != http.StatusNotFound || got.raw != missingResult.raw || time.Since(start) > 2*time.Second {
			s.t.Fatalf("%s GET result: %d %s after %s, want immediate %s", name, got.status, got.raw, time.Since(start), missingResult.raw)
		}
	}
	// Legacy static tokens keep reading their own tenant.
	s.expect(s.call(http.MethodGet, "/v1/codeq/tasks/"+localID, staticProducerTok, nil), http.StatusOK, "", "static own tenant")
	s.expect(s.call(http.MethodGet, "/v1/codeq/tasks/"+localID, staticWorkerTok, nil), http.StatusOK, "", "static worker own tenant")
	s.expect(s.call(http.MethodGet, "/v1/codeq/tasks/"+localID, publish, nil), http.StatusNotFound, "not found", "binding reads local-tenant")
}

func (s *bindingSuite) claim(token string) apiResult {
	s.t.Helper()
	return s.call(http.MethodPost, pathClaim, token, map[string]any{"leaseSeconds": 60})
}

func (s *bindingSuite) drain(token string) {
	for s.claim(token).status == http.StatusOK {
		continue
	}
}

func (s *bindingSuite) workerOwnership(publish, subscribe string) {
	s.drain(subscribe)
	id := s.createTask(publish, bindingTopic)
	r := s.call(http.MethodPost, pathClaim, subscribe, map[string]any{keyCommands: []string{"other-topic"}})
	s.expect(r, http.StatusForbidden, "event type not allowed", "claim other command")
	r = s.claim(subscribe)
	s.expect(r, http.StatusOK, "", "claim")
	if r.body["id"] != id || r.body["tenantId"] != tenantConveste {
		s.t.Fatalf("claimed unexpected task: %s", r.raw)
	}
	otherPod := s.bindingToken(policySubscribe, tenantConveste, bindingTopic, "pod-2", nil)
	otherTenant := s.bindingToken(policySubscribe, "other", bindingTopic, "pod-1", nil)
	for name, token := range map[string]string{"other pod": otherPod, "other tenant": otherTenant} {
		for _, action := range []string{"heartbeat", "abandon", "nack", keyResult} {
			r = s.call(http.MethodPost, "/v1/codeq/tasks/"+id+"/"+action, token, map[string]any{keyStatus: statusCompleted, keyResult: okResult})
			s.expect(r, http.StatusForbidden, "not-owner", name+" "+action)
		}
		r = s.call(http.MethodPost, "/v1/codeq/tasks/does-not-exist/heartbeat", token, map[string]any{})
		s.expect(r, http.StatusForbidden, "not-owner", name+" missing task")
	}
	s.expect(s.call(http.MethodPost, "/v1/codeq/tasks/"+id+"/heartbeat", subscribe, map[string]any{"extendSeconds": 30}), http.StatusOK, "", "owner heartbeat")
	s.expect(s.call(http.MethodPost, "/v1/codeq/tasks/"+id+"/abandon", subscribe, nil), http.StatusOK, "", "owner abandon")
	r = s.claim(subscribe)
	s.expect(r, http.StatusOK, "", "re-claim")
	if r.body["id"] != id {
		s.t.Fatalf("re-claim returned %v, want %s", r.body["id"], id)
	}
	second := s.createTask(publish, bindingTopic)
	r = s.call(http.MethodPost, "/v1/codeq/tasks/batch/results", subscribe, map[string]any{"results": []any{
		map[string]any{"taskId": second, keyStatus: statusCompleted, keyResult: okResult},
		map[string]any{"taskId": id, keyStatus: statusCompleted, keyResult: okResult},
	}})
	s.expect(r, http.StatusOK, "", "batch results")
	results, _ := r.body["results"].([]any)
	if len(results) != 2 || results[0].(map[string]any)["error"] != "not-owner" || results[1].(map[string]any)["error"] != nil {
		s.t.Fatalf("batch results not per-item owner checked: %s", r.raw)
	}
	s.expect(s.call(http.MethodGet, "/v1/codeq/tasks/"+id+"/result", publish, nil), http.StatusOK, "", "publisher reads result")
	// The unclaimed task cannot be completed by a binding worker (strict owner).
	r = s.call(http.MethodPost, "/v1/codeq/tasks/"+second+"/result", subscribe, map[string]any{keyStatus: statusCompleted, keyResult: okResult})
	s.expect(r, http.StatusForbidden, "not-owner", "unclaimed result")
	r = s.claim(subscribe)
	s.expect(r, http.StatusOK, "", "claim second")
	s.expect(s.call(http.MethodPost, "/v1/codeq/tasks/"+second+"/nack", subscribe, map[string]any{"delaySeconds": 5}), http.StatusOK, "", "owner nack")
}

func (s *bindingSuite) routeAllowList(publish, subscribe string) {
	refused := []struct{ method, path string }{
		{http.MethodGet, "/v1/codeq/admin/queues"},
		{http.MethodGet, "/v1/codeq/admin/queues/" + bindingTopic},
		{http.MethodPost, "/v1/codeq/admin/tasks/cleanup"},
		{http.MethodPut, "/v1/codeq/admin/topics/" + bindingTopic},
		{http.MethodGet, "/v1/codeq/admin/topics/" + bindingTopic},
		{http.MethodDelete, "/v1/codeq/admin/topics/" + bindingTopic},
	}
	for _, rc := range refused {
		s.expect(s.call(rc.method, rc.path, publish, map[string]any{}), http.StatusForbidden, "route_not_allowed", "publish "+rc.path)
		s.expect(s.call(rc.method, rc.path, subscribe, map[string]any{}), http.StatusUnauthorized, "", "subscribe "+rc.path)
	}
	sub := map[string]any{"callbackUrl": "http://169.254.169.254/", claimEventTypes: []string{bindingTopic}}
	s.expect(s.call(http.MethodPost, "/v1/codeq/workers/subscriptions", subscribe, sub), http.StatusForbidden, "route_not_allowed", "subscribe subscriptions")
	s.expect(s.call(http.MethodPost, "/v1/codeq/workers/subscriptions/x/heartbeat", subscribe, map[string]any{}), http.StatusForbidden, "route_not_allowed", "subscription heartbeat")
	s.expect(s.call(http.MethodPost, "/v1/codeq/workers/subscriptions", publish, sub), http.StatusUnauthorized, "", "publish subscriptions")
	for _, path := range []string{pathClaim, "/v1/codeq/tasks/claim/batch", "/v1/codeq/tasks/x/heartbeat", "/v1/codeq/tasks/batch/results"} {
		s.expect(s.call(http.MethodPost, path, publish, map[string]any{}), http.StatusUnauthorized, "", "publish "+path)
	}
	for _, path := range []string{pathTasks, "/v1/codeq/tasks/batch"} {
		s.expect(s.call(http.MethodPost, path, subscribe, map[string]any{}), http.StatusUnauthorized, "", "subscribe "+path)
	}
	for _, token := range []string{publish, subscribe} {
		if r := s.call(http.MethodGet, "/v1/codeq/raft/status", token, nil); r.status != http.StatusOK {
			s.t.Fatalf("raft status refused: %d %s", r.status, r.raw)
		}
	}
}

func (s *bindingSuite) invalidTokens() {
	cases := map[string]struct {
		policy string
		mutate func(map[string]any)
		status int
	}{
		"wildcard event type": {policySubscribe, func(c map[string]any) {
			c["eventTypes"] = []string{"*"}
			c["codeq_binding"].(map[string]any)["topicId"] = "conveste.*"
		}, http.StatusForbidden},
		"role claim":         {policyPublish, func(c map[string]any) { c["role"] = "ADMIN" }, http.StatusForbidden},
		"admin scope":        {policyPublish, func(c map[string]any) { c["scope"] = "codeq:publish codeq:admin" }, http.StatusForbidden},
		"subscribe scope":    {policySubscribe, func(c map[string]any) { c["scope"] = bindingWorkerScope + " codeq:subscribe" }, http.StatusForbidden},
		"tenant alias":       {policyPublish, func(c map[string]any) { c["tenantId"] = tenantConveste }, http.StatusForbidden},
		"topic mismatch":     {policyPublish, func(c map[string]any) { c["codeq_binding"].(map[string]any)["topicId"] = "other." + bindingTopic }, http.StatusForbidden},
		"lifetime over 300s": {policyPublish, func(c map[string]any) { c[claimExpiry] = c[claimIssuedAt].(int64) + 400 }, http.StatusForbidden},
		"expired": {policyPublish, func(c map[string]any) {
			c[claimIssuedAt], c[claimExpiry] = time.Now().Unix()-400, time.Now().Unix()-120
		}, http.StatusUnauthorized},
		"publish w/o binding": {policyPublish, func(c map[string]any) { delete(c, "codeq_binding") }, http.StatusForbidden},
	}
	for name, tc := range cases {
		token := s.bindingToken(tc.policy, tenantConveste, bindingTopic, "pod-x", tc.mutate)
		path, body := pathTasks, any(map[string]any{keyCommand: bindingTopic, keyPayload: 1})
		if tc.policy == policySubscribe {
			path, body = pathClaim, map[string]any{}
		}
		code := ""
		if tc.status == http.StatusForbidden {
			code = "binding_scope_denied"
		}
		s.expect(s.call(http.MethodPost, path, token, body), tc.status, code, name)
		s.expect(s.call(http.MethodGet, "/v1/codeq/admin/queues", token, nil), map[bool]int{true: http.StatusUnauthorized, false: tc.status}[tc.policy == policySubscribe], "", name+" admin")
	}
}

func (s *bindingSuite) legacyUnchanged() {
	s.expect(s.call(http.MethodGet, "/v1/codeq/admin/queues", staticProducerTok, nil), http.StatusOK, "", "static admin queues")
	s.expect(s.call(http.MethodGet, "/v1/codeq/admin/queues", s.adminToken("local-tenant"), nil), http.StatusOK, "", "platform admin queues")
	id := s.createTask(staticProducerTok, "legacy-command")
	r := s.call(http.MethodPost, pathClaim, staticWorkerTok, map[string]any{keyCommands: []string{"legacy-command"}})
	s.expect(r, http.StatusOK, "", "static worker claim")
	s.expect(s.call(http.MethodPost, "/v1/codeq/tasks/"+id+"/result", staticWorkerTok, map[string]any{keyStatus: statusCompleted, keyResult: okResult}), http.StatusOK, "", "static worker result")
	r = s.call(http.MethodPost, pathTasks, staticProducerTok, map[string]any{keyCommand: "legacy-command", keyPayload: 1, keyWebhook: "https://example.com/hook"})
	s.expect(r, http.StatusAccepted, "", "static producer webhook unchanged")
}

func (s *bindingSuite) grpcStreams(publish, subscribe string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	workerConn, err := grpc.NewClient(s.workerAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.t.Fatalf("dial worker: %v", err)
	}
	defer workerConn.Close()
	for name, token := range map[string]string{"subscribe": subscribe, "publish": publish} {
		stream, err := workerpb.NewWorkerStreamClient(workerConn).Stream(ctx)
		if err != nil {
			s.t.Fatalf("worker stream: %v", err)
		}
		_ = stream.Send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Hello{Hello: &workerpb.Hello{Token: token}}})
		_, err = stream.Recv()
		if status.Code(err) == codes.OK || (name == "subscribe" && status.Code(err) != codes.PermissionDenied) {
			s.t.Fatalf("worker stream accepted %s token: %v", name, err)
		}
	}
	stream, err := workerpb.NewWorkerStreamClient(workerConn).Stream(ctx)
	if err != nil {
		s.t.Fatalf("worker stream: %v", err)
	}
	_ = stream.Send(&workerpb.WorkerEvent{Event: &workerpb.WorkerEvent_Hello{Hello: &workerpb.Hello{Token: staticWorkerTok}}})
	if _, err := stream.Recv(); err != nil {
		s.t.Fatalf("static worker stream refused: %v", err)
	}
	prodConn, err := grpc.NewClient(s.prodAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		s.t.Fatalf("dial producer: %v", err)
	}
	defer prodConn.Close()
	pstream, err := producerpb.NewProducerStreamClient(prodConn).Stream(ctx)
	if err != nil {
		s.t.Fatalf("producer stream: %v", err)
	}
	_ = pstream.Send(&producerpb.ProducerEvent{Event: &producerpb.ProducerEvent_Hello{Hello: &producerpb.Hello{Token: publish}}})
	if _, err := pstream.Recv(); status.Code(err) != codes.PermissionDenied || !strings.Contains(err.Error(), "route_not_allowed") {
		s.t.Fatalf("producer stream accepted a binding token: %v", err)
	}
}
