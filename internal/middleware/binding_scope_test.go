package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/osvaldoandrade/codeq/internal/authclaims"
	"github.com/osvaldoandrade/codeq/pkg/auth"
	"github.com/osvaldoandrade/codeq/pkg/config"
)

type fakeValidator map[string]*auth.Claims

func (f fakeValidator) Validate(token string) (*auth.Claims, error) {
	if claims, ok := f[token]; ok {
		return claims, nil
	}
	return nil, errors.New("invalid token")
}

const (
	mwTenant          = "conveste"
	envTest           = "test"
	claimAud          = "aud"
	roleAdmin         = "ADMIN"
	tokLegacyProducer = "legacy-producer"
	tokLegacyWorker   = "legacy-worker"
	pathTask1         = routeTasks + "/t1"
	pathSubscriptions = "/v1/codeq/workers/subscriptions"
	pathTopic         = "/v1/codeq/admin/topics/cflow-executar"
)

const (
	claimEventTypes = "eventTypes"
	claimRole       = "role"
	tokPublish      = "publish"
	tokSubscribe    = "subscribe"
)

const claimScope = "scope"

const bindingWorkerScopes = "codeq:abandon codeq:claim codeq:heartbeat codeq:nack codeq:result"

func testBindingClaims(policy authclaims.BindingPolicy, eventType string) *auth.Claims {
	tenant := mwTenant
	aud, scope := authclaims.BindingProducerAudience, authclaims.PublishScope
	if policy == authclaims.PolicySubscribe {
		aud, scope = authclaims.BindingWorkerAudience, bindingWorkerScopes
	}
	now := time.Now()
	return &auth.Claims{
		Subject:    authclaims.BindingSubjectPrefix + "k3s:workload-" + tenant + ":sa:pod-1",
		Audience:   []string{aud},
		Scopes:     strings.Fields(scope),
		EventTypes: []string{eventType},
		IssuedAt:   now.Add(-5 * time.Second),
		ExpiresAt:  now.Add(295 * time.Second),
		Raw: map[string]interface{}{
			claimAud: aud, testClaimTID: tenant, claimScope: scope, claimEventTypes: []interface{}{eventType},
			authclaims.BindingClaim: map[string]interface{}{
				"uid": "uid-1", "generation": float64(1), "policy": string(policy), "topicId": tenant + "." + eventType,
			},
		},
	}
}

func legacyProducerClaims() *auth.Claims {
	return &auth.Claims{Subject: "codecloud-producer", Raw: map[string]interface{}{claimRole: roleAdmin, testClaimTenantID: "local-tenant"}}
}

func legacyWorkerClaims() *auth.Claims {
	return &auth.Claims{
		Subject: "codecloud-worker", EventTypes: []string{"*"},
		Scopes: promotedWorkerScopes,
		Raw:    map[string]interface{}{testClaimTenantID: "local-tenant"},
	}
}

type routerFixture struct {
	engine   *gin.Engine
	producer fakeValidator
	worker   fakeValidator
}

// newBindingRouter registers the production route templates with
// pass-through handlers in the same middleware order as SetupMappings.
func newBindingRouter(cfg *config.Config) *routerFixture {
	gin.SetMode(gin.TestMode)
	publish := testBindingClaims(authclaims.PolicyPublish, "cflow-executar")
	subscribe := testBindingClaims(authclaims.PolicySubscribe, "cflow-executar")
	f := &routerFixture{
		engine:   gin.New(),
		producer: fakeValidator{tokPublish: publish, tokLegacyProducer: legacyProducerClaims()},
		worker:   fakeValidator{tokSubscribe: subscribe, tokLegacyWorker: legacyWorkerClaims()},
	}
	ok := func(c *gin.Context) {
		_, binding := GetBindingScope(c)
		c.JSON(http.StatusOK, gin.H{"binding": binding, claimRole: c.GetString("userRole"), "tenant": GetTenantID(c)})
	}
	v1 := f.engine.Group("/v1/codeq")
	producer := v1.Group("", AuthMiddleware(f.producer, cfg))
	worker := v1.Group("", WorkerAuthMiddleware(f.worker, f.producer, cfg))
	anyAuth := v1.Group("", AnyAuthMiddleware(f.worker, f.producer, cfg))
	producer.POST("/tasks", ok)
	producer.POST("/tasks/batch", ok)
	for _, p := range []string{"/tasks/claim", "/tasks/claim/batch", "/tasks/:id/heartbeat", "/tasks/:id/progress", "/tasks/:id/abandon", "/tasks/:id/nack", "/tasks/:id/result", "/tasks/batch/results"} {
		worker.POST(p, ok)
	}
	worker.POST("/workers/subscriptions", RequireWorkerScope("codeq:subscribe"), ok)
	worker.POST("/workers/subscriptions/:id/heartbeat", RequireWorkerScope("codeq:subscribe"), ok)
	anyAuth.GET("/tasks/:id", ok)
	anyAuth.GET("/tasks/:id/result", ok)
	anyAuth.GET("/raft/status", ok)
	admin := producer.Group("/admin", RequireAdmin())
	topicAdmin := producer.Group("/admin", RequireTopicAdmin())
	admin.GET("/queues", ok)
	admin.POST("/tasks/cleanup", ok)
	topicAdmin.GET("/queues/:command", ok)
	topicAdmin.PUT("/topics/:topicName", ok)
	topicAdmin.GET("/topics/:topicName", ok)
	topicAdmin.DELETE("/topics/:topicName", ok)
	return f
}

func (f *routerFixture) do(t *testing.T, method, path, token string, header ...string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	f.engine.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

type routeCase struct {
	method, path string
	publish      int
	subscribe    int
}

// bindingRouteMatrix is ADR-0022 C1.2 at the middleware layer. 401 rows are
// rejected by the audience-separated validators before the allow-list.
var bindingRouteMatrix = []routeCase{
	{http.MethodPost, routeTasks, http.StatusOK, http.StatusUnauthorized},
	{http.MethodPost, "/v1/codeq/tasks/batch", http.StatusOK, http.StatusUnauthorized},
	{http.MethodGet, pathTask1, http.StatusOK, http.StatusOK},
	{http.MethodGet, pathTask1 + "/result", http.StatusOK, http.StatusOK},
	{http.MethodGet, "/v1/codeq/raft/status", http.StatusOK, http.StatusOK},
	{http.MethodPost, routeTasksClaim, http.StatusUnauthorized, http.StatusOK},
	{http.MethodPost, "/v1/codeq/tasks/claim/batch", http.StatusUnauthorized, http.StatusOK},
	{http.MethodPost, "/v1/codeq/tasks/t1/heartbeat", http.StatusUnauthorized, http.StatusOK},
	{http.MethodPost, "/v1/codeq/tasks/t1/progress", http.StatusUnauthorized, http.StatusOK},
	{http.MethodPost, "/v1/codeq/tasks/t1/abandon", http.StatusUnauthorized, http.StatusOK},
	{http.MethodPost, "/v1/codeq/tasks/t1/nack", http.StatusUnauthorized, http.StatusOK},
	{http.MethodPost, pathTask1 + "/result", http.StatusUnauthorized, http.StatusOK},
	{http.MethodPost, "/v1/codeq/tasks/batch/results", http.StatusUnauthorized, http.StatusOK},
	{http.MethodPost, pathSubscriptions, http.StatusUnauthorized, http.StatusForbidden},
	{http.MethodPost, "/v1/codeq/workers/subscriptions/s1/heartbeat", http.StatusUnauthorized, http.StatusForbidden},
	{http.MethodGet, "/v1/codeq/admin/queues", http.StatusForbidden, http.StatusUnauthorized},
	{http.MethodPost, "/v1/codeq/admin/tasks/cleanup", http.StatusForbidden, http.StatusUnauthorized},
	{http.MethodGet, "/v1/codeq/admin/queues/cflow-executar", http.StatusForbidden, http.StatusUnauthorized},
	{http.MethodPut, pathTopic, http.StatusForbidden, http.StatusUnauthorized},
	{http.MethodGet, pathTopic, http.StatusForbidden, http.StatusUnauthorized},
	{http.MethodDelete, pathTopic, http.StatusForbidden, http.StatusUnauthorized},
}

func TestBindingRouteAllowList(t *testing.T) {
	f := newBindingRouter(&config.Config{Env: envTest})
	for _, rc := range bindingRouteMatrix {
		for token, want := range map[string]int{tokPublish: rc.publish, tokSubscribe: rc.subscribe} {
			code, body := f.do(t, rc.method, rc.path, token)
			if code != want {
				t.Fatalf("%s %s with %s: status %d, want %d (body %v)", rc.method, rc.path, token, code, want, body)
			}
			if code == http.StatusForbidden && body["error"] != CodeRouteNotAllowed {
				t.Fatalf("%s %s with %s: error %v, want %s", rc.method, rc.path, token, body["error"], CodeRouteNotAllowed)
			}
			if code == http.StatusOK && body["binding"] != true {
				t.Fatalf("%s %s with %s: binding scope missing from context", rc.method, rc.path, token)
			}
		}
	}
}

func TestBindingRouteAllowListCoversEveryEntry(t *testing.T) {
	for route, policies := range bindingRoutes {
		if len(policies) == 0 || !strings.HasPrefix(route.path, "/v1/codeq/") || strings.Contains(route.path, "/admin") {
			t.Fatalf("unexpected allow-list entry %+v", route)
		}
	}
	if bindingRouteAllowed(http.MethodPost, routeTasks, authclaims.PolicySubscribe) {
		t.Fatal("Subscribe must not create tasks")
	}
	if bindingRouteAllowed(http.MethodDelete, "/v1/codeq/tasks/:id", authclaims.PolicyPublish) {
		t.Fatal("unlisted method allowed")
	}
}

// TestLegacyTokensUnchangedByBindingGuard proves static and admin tokens are
// not routed through the allow-list.
func TestLegacyTokensUnchangedByBindingGuard(t *testing.T) {
	f := newBindingRouter(&config.Config{Env: envTest})
	cases := []struct {
		method, path, token string
	}{
		{http.MethodPost, routeTasks, tokLegacyProducer},
		{http.MethodGet, "/v1/codeq/admin/queues", tokLegacyProducer},
		{http.MethodPost, "/v1/codeq/admin/tasks/cleanup", tokLegacyProducer},
		{http.MethodGet, pathTask1, tokLegacyProducer},
		{http.MethodPost, pathSubscriptions, tokLegacyWorker},
		{http.MethodPost, routeTasksClaim, tokLegacyWorker},
		{http.MethodGet, pathTask1, tokLegacyWorker},
	}
	for _, tc := range cases {
		code, body := f.do(t, tc.method, tc.path, tc.token)
		if code != http.StatusOK || body["binding"] != false {
			t.Fatalf("%s %s with %s: status %d body %v", tc.method, tc.path, tc.token, code, body)
		}
	}
}

func TestInvalidBindingTokenDeniedOnEveryRoute(t *testing.T) {
	f := newBindingRouter(&config.Config{Env: envTest})
	badPublish := testBindingClaims(authclaims.PolicyPublish, "cflow-executar")
	badPublish.Raw[claimRole] = roleAdmin
	f.producer["bad-publish"] = badPublish
	badSubscribe := testBindingClaims(authclaims.PolicySubscribe, "*")
	f.worker["bad-subscribe"] = badSubscribe
	noEvents := testBindingClaims(authclaims.PolicySubscribe, "cflow-executar")
	noEvents.EventTypes, noEvents.Raw[claimEventTypes] = nil, []interface{}{}
	f.worker["no-events"] = noEvents
	publishWithoutBinding := legacyProducerClaims()
	publishWithoutBinding.Scopes = []string{authclaims.PublishScope}
	f.producer["publish-without-binding"] = publishWithoutBinding

	before := deniedCount(t, authclaims.ReasonForbiddenClaim, "/v1/codeq/admin/queues")
	for _, rc := range bindingRouteMatrix {
		for token, applies := range map[string]bool{
			"bad-publish": rc.publish != http.StatusUnauthorized, "publish-without-binding": rc.publish != http.StatusUnauthorized,
			"bad-subscribe": rc.subscribe != http.StatusUnauthorized, "no-events": rc.subscribe != http.StatusUnauthorized,
		} {
			if !applies {
				continue
			}
			code, body := f.do(t, rc.method, rc.path, token)
			if code != http.StatusForbidden || body["error"] != CodeBindingScopeDenied {
				t.Fatalf("%s %s with %s: status %d body %v, want 403 %s", rc.method, rc.path, token, code, body, CodeBindingScopeDenied)
			}
		}
	}
	after := deniedCount(t, authclaims.ReasonForbiddenClaim, "/v1/codeq/admin/queues")
	if after != before+1 {
		t.Fatalf("denial metric delta = %v, want 1", after-before)
	}
}

func TestProducerAsWorkerNeverPromotesBindingTokens(t *testing.T) {
	f := newBindingRouter(&config.Config{Env: envTest, AllowProducerAsWorker: true, WorkerAudience: "codeq-worker"})
	publishWithoutBinding := legacyProducerClaims()
	publishWithoutBinding.Scopes = []string{authclaims.PublishScope}
	f.producer["publish-without-binding"] = publishWithoutBinding
	for _, token := range []string{tokPublish, "publish-without-binding"} {
		for _, path := range []string{routeTasksClaim, pathTask1 + "/result", pathSubscriptions} {
			if code, body := f.do(t, http.MethodPost, path, token); code != http.StatusUnauthorized {
				t.Fatalf("%s promoted on %s: status %d body %v", token, path, code, body)
			}
		}
	}
	// The legacy producer is still promoted, as before.
	if code, body := f.do(t, http.MethodPost, routeTasksClaim, tokLegacyProducer); code != http.StatusOK {
		t.Fatalf("legacy producer promotion broken: %d %v", code, body)
	}
}

func TestBindingTokenIgnoresDevRoleHeader(t *testing.T) {
	f := newBindingRouter(&config.Config{Env: "dev"})
	code, body := f.do(t, http.MethodPost, routeTasks, tokPublish, "X-Role", roleAdmin)
	if code != http.StatusOK || body[claimRole] != "USER" {
		t.Fatalf("binding token took X-Role: %d %v", code, body)
	}
	code, _ = f.do(t, http.MethodGet, "/v1/codeq/admin/queues", tokPublish, "X-Role", roleAdmin)
	if code != http.StatusForbidden {
		t.Fatalf("binding token reached admin with X-Role: %d", code)
	}
}

func TestBindingTenantComesFromTid(t *testing.T) {
	f := newBindingRouter(&config.Config{Env: envTest})
	code, body := f.do(t, http.MethodPost, routeTasks, tokPublish, "X-Tenant-Id", "local-tenant")
	if code != http.StatusOK || body["tenant"] != mwTenant {
		t.Fatalf("tenant = %v (status %d), want conveste", body["tenant"], code)
	}
}

func TestDenyBindingBoundsRequestIDAndAddsExtra(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.POST("/v1/codeq/tasks/batch", func(c *gin.Context) {
		c.Set("request_id", strings.Repeat("r", 400))
		DenyBinding(c, &authclaims.BindingScope{TenantID: mwTenant}, http.StatusForbidden, CodeEventTypeNotAllow, "event_type_not_allowed", gin.H{"index": 3})
	})
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/codeq/tasks/batch", nil))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusForbidden || body["error"] != CodeEventTypeNotAllow || body["index"] != float64(3) {
		t.Fatalf("unexpected denial %d %v", rec.Code, body)
	}
}

// deniedCount reads codeq_binding_scope_denied_total{reason,route} from the
// default registry, where the metrics package registers it.
func deniedCount(t *testing.T, reason, route string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "codeq_binding_scope_denied_total" {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := map[string]string{}
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			if labels["reason"] == reason && labels["route"] == route {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}
