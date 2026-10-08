package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PNT-Data-Center/omnibusmcp/internal/audit"
	"github.com/PNT-Data-Center/omnibusmcp/internal/certs"
	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
	"github.com/PNT-Data-Center/omnibusmcp/internal/executor"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
	"github.com/PNT-Data-Center/omnibusmcp/internal/tier"
)

const testToken = "0123456789abcdef0123456789abcdef" // gitleaks:allow (fake test data)

// fakeModule has one tool per tier and a fixed health verdict.
type fakeModule struct{}

func (fakeModule) Name() string                               { return "fake" }
func (fakeModule) Description() string                        { return "test module" }
func (fakeModule) Detect(context.Context, *registry.Env) bool { return true }
func (fakeModule) Health(context.Context, *registry.Env) []registry.Check {
	return []registry.Check{{Module: "fake", Name: "thing", Status: registry.Warn, Detail: "half broken"}}
}

type echoIn struct {
	Text string `json:"text"`
}

func (fakeModule) Tools(*registry.Env) []registry.Tool {
	return []registry.Tool{
		registry.NewTool("fake_read", "Read", "read-only", tier.ReadOnly, true,
			func(_ context.Context, in echoIn) (string, error) { return "read:" + in.Text, nil }),
		registry.NewTool("fake_restart", "Restart", "tier 2", tier.ServiceControl, false,
			func(context.Context, echoIn) (string, error) { return "restarted", nil }),
	}
}

type bearer struct{ scheme, token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b.token != "" {
		r.Header.Set("Authorization", b.scheme+" "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func startServer(t *testing.T, cur tier.Tier) string {
	t.Helper()
	return startServerAudit(t, cur, audit.Discard())
}

func startServerAudit(t *testing.T, cur tier.Tier, a *audit.Logger) string {
	t.Helper()
	cfg := config.Default()
	cfg.Tier = cur
	env := &registry.Env{Cfg: cfg, Exec: executor.New(5*time.Second, 64*1024)}
	s, err := NewMCPServer(Options{Env: env, Modules: []registry.Module{fakeModule{}}, Audit: a, Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(Handler(s, testToken, a, nil))
	t.Cleanup(ts.Close)
	return ts.URL + "/mcp"
}

func connect(t *testing.T, url, token string) (*mcp.ClientSession, error) {
	t.Helper()
	return connectScheme(t, url, "Bearer", token)
}

func connectScheme(t *testing.T, url, scheme, token string) (*mcp.ClientSession, error) {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tr := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer{scheme, token}}, MaxRetries: -1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Connect(ctx, tr, nil)
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
	}
	return names
}

func TestAuthRequired(t *testing.T) {
	url := startServer(t, tier.ReadOnly)
	for _, tok := range []string{"", "wrong-token-wrong-token-wrong-token"} {
		if cs, err := connect(t, url, tok); err == nil {
			cs.Close()
			t.Fatalf("connected with token %q", tok)
		}
	}
}

func TestBearerSchemeCaseInsensitive(t *testing.T) {
	url := startServer(t, tier.ReadOnly)
	for _, scheme := range []string{"bearer", "BEARER"} {
		cs, err := connectScheme(t, url, scheme, testToken)
		if err != nil {
			t.Fatalf("%s rejected: %v", scheme, err)
		}
		cs.Close()
	}
	if cs, err := connectScheme(t, url, "Basic", testToken); err == nil {
		cs.Close()
		t.Fatal("Basic accepted")
	}
}

func TestAuditCoversRejectedCalls(t *testing.T) {
	var buf syncBuffer
	cs, err := connect(t, startServerAudit(t, tier.ReadOnly, audit.New(&buf)), testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ctx := context.Background()
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "fake_read", Arguments: map[string]any{"text": "x"}})
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "fake_read", Arguments: map[string]any{"text": 5}})
	cs.CallTool(ctx, &mcp.CallToolParams{Name: "run_command", Arguments: map[string]any{"cmd": "id"}})

	var statuses []string
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var e struct {
			Msg    string          `json:"msg"`
			Tool   string          `json:"tool"`
			Status string          `json:"status"`
			Args   json.RawMessage `json:"args"`
		}
		if json.Unmarshal([]byte(line), &e) == nil && e.Msg == "tool_call" {
			statuses = append(statuses, e.Tool+"="+e.Status)
		}
	}
	if got := strings.Join(statuses, ","); got != "fake_read=ok,fake_read=error,run_command=rejected" {
		t.Fatalf("audited calls: %s\n%s", got, buf.String())
	}
	if !strings.Contains(buf.String(), `"args":{"cmd":"id"}`) {
		t.Fatalf("raw args not audited:\n%s", buf.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestTierFiltering(t *testing.T) {
	cs, err := connect(t, startServer(t, tier.ReadOnly), testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	got := strings.Join(toolNames(t, cs), ",")
	if got != "fake_read,health_summary" {
		t.Fatalf("tier 1 tools = %s", got)
	}

	cs2, err := connect(t, startServer(t, tier.ServiceControl), testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs2.Close()
	if got := strings.Join(toolNames(t, cs2), ","); got != "fake_read,fake_restart,health_summary" {
		t.Fatalf("tier 2 tools = %s", got)
	}
}

func TestCallTools(t *testing.T) {
	cs, err := connect(t, startServer(t, tier.ReadOnly), testToken)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	ctx := context.Background()

	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "health_summary"})
	if err != nil || res.IsError {
		t.Fatalf("health_summary: %v %+v", err, res)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(text, "OVERALL: WARN") || !strings.Contains(text, "fake/thing: half broken") {
		t.Fatalf("health output:\n%s", text)
	}

	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "fake_read", Arguments: map[string]any{"text": "x"}})
	if err != nil || res.Content[0].(*mcp.TextContent).Text != "read:x" {
		t.Fatalf("fake_read: %v %+v", err, res)
	}

	// Hidden tier-2 tool cannot be called at tier 1.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "fake_restart", Arguments: map[string]any{"text": "x"}})
	if err == nil && !res.IsError {
		t.Fatal("tier 2 tool callable at tier 1")
	}

	rr, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "omnibus://server/info"})
	if err != nil || !strings.Contains(rr.Contents[0].Text, `"tier": 1`) {
		t.Fatalf("server info: %v", err)
	}
}

func TestTLSCheck(t *testing.T) {
	now := time.Now()
	cfg := config.Default()
	if c := tlsCheck(cfg, now); c.Status != registry.OK || !strings.Contains(c.Detail, "disabled") {
		t.Fatalf("%+v", c)
	}
	d := t.TempDir()
	cfg.TLS.CertFile, cfg.TLS.KeyFile = d+"/c.pem", d+"/k.pem"
	if c := tlsCheck(cfg, now); c.Status != registry.Crit {
		t.Fatalf("missing cert: %+v", c)
	}
	if _, err := certs.Generate(TLSPaths(cfg), []string{"localhost"}, 243, now); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		at   time.Time
		want registry.Status
	}{
		{now, registry.OK},
		{now.Add(220 * 24 * time.Hour), registry.Warn},
		{now.Add(250 * 24 * time.Hour), registry.Crit},
	}
	for _, tc := range cases {
		if c := tlsCheck(cfg, tc.at); c.Status != tc.want {
			t.Errorf("at %s: %+v", tc.at, c)
		}
	}
}

func TestServeTLS(t *testing.T) {
	d := t.TempDir()
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.TLS.CertFile, cfg.TLS.KeyFile = d+"/c.pem", d+"/k.pem"
	if _, err := LoadTLS(cfg); err == nil {
		t.Fatal("missing certificate accepted")
	}
	if _, err := certs.Generate(TLSPaths(cfg), []string{"127.0.0.1"}, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	tc, err := LoadTLS(cfg)
	if err != nil || tc.MinVersion != tls.VersionTLS12 {
		t.Fatalf("%v %+v", err, tc)
	}
}

// The CA is downloadable without a token, and a client trusting only that
// CA completes a TLS handshake with the server.
func TestCAEndpoint(t *testing.T) {
	d := t.TempDir()
	cfg := config.Default()
	cfg.TLS.CertFile, cfg.TLS.KeyFile = d+"/c.pem", d+"/k.pem"
	if _, err := certs.Generate(TLSPaths(cfg), []string{"127.0.0.1"}, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	tc, err := LoadTLS(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a, err := audit.Open(d + "/audit.log")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ts := httptest.NewUnstartedServer(Handler(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil), testToken, a, &Public{CAFile: cfg.TLS.CAFile(), TokenFile: "/etc/omnibusmcp/token", Listen: "127.0.0.1:8765"}))
	ts.TLS = tc
	ts.StartTLS()
	defer ts.Close()

	// Download as a first-time client would: without trusting anything yet.
	insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := insecure.Get(ts.URL + CAPath)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/x-pem-file" {
		t.Fatalf("GET %s: %d %q", CAPath, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		t.Fatalf("not a PEM certificate: %q", body)
	}
	trusting := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err = trusting.Get(ts.URL + "/mcp")
	if err != nil {
		t.Fatalf("TLS with the downloaded CA: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("/mcp without token: %d", resp.StatusCode)
	}

	// The landing page uses the host the client asked for and shows the CA.
	resp, err = insecure.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	host := strings.TrimPrefix(ts.URL, "https://")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(page), "https://"+host+"/ca.pem") {
		t.Fatalf("landing page %d:\n%s", resp.StatusCode, page)
	}
	// A forged Host header is not echoed into the commands.
	req, _ := http.NewRequest("GET", ts.URL+"/", nil)
	req.Host = "evil.example;rm -rf /"
	resp, err = insecure.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	page, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(page), "rm -rf") || !strings.Contains(string(page), "https://127.0.0.1:8765/ca.pem") {
		t.Fatalf("forged host:\n%s", page)
	}
	// Other paths stay unknown.
	if resp, err := insecure.Get(ts.URL + "/admin"); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/admin: %v %v", resp, err)
	}

	// A file that is not an OmnibusMCP CA is never served.
	ts2 := httptest.NewServer(Handler(mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil), testToken, a, &Public{CAFile: cfg.TLS.CertFile}))
	defer ts2.Close()
	if resp, err := http.Get(ts2.URL + CAPath); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("foreign file served: %v %v", resp, err)
	}
}
