// Package server wires modules into an MCP server and serves it over
// Streamable HTTP behind Bearer authentication.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/PNT-Data-Center/omnibusmcp/internal/audit"
	"github.com/PNT-Data-Center/omnibusmcp/internal/certs"
	"github.com/PNT-Data-Center/omnibusmcp/internal/clientsetup"
	"github.com/PNT-Data-Center/omnibusmcp/internal/compat"
	"github.com/PNT-Data-Center/omnibusmcp/internal/config"
	"github.com/PNT-Data-Center/omnibusmcp/internal/registry"
	"github.com/PNT-Data-Center/omnibusmcp/internal/tier"
)

// Options configure the server.
type Options struct {
	Env     *registry.Env
	Modules []registry.Module
	Audit   *audit.Logger
	Version string
}

// Tools returns every tool the server exposes at the configured tier,
// including the server-level ones.
func Tools(o Options) ([]registry.Tool, error) {
	tools, err := registry.Collect(o.Env, o.Modules, o.Env.Cfg.Tier)
	if err != nil {
		return nil, err
	}
	return append([]registry.Tool{healthTool(o)}, tools...), nil
}

// NewMCPServer builds the MCP server with the tools, prompts and resources
// allowed by the configuration.
func NewMCPServer(o Options) (*mcp.Server, error) {
	tools, err := Tools(o)
	if err != nil {
		return nil, err
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "omnibusmcp", Title: "OmnibusMCP", Version: o.Version},
		&mcp.ServerOptions{Instructions: instructions(o, tools)})

	s.AddReceivingMiddleware(auditMiddleware(o.Audit))
	hook := tierHook(o.Env.Cfg.Tier)
	for i := range tools {
		tools[i].Install(s, hook)
	}
	addPrompts(s, o)
	addResources(s, o, tools)
	return s, nil
}

// tierHook enforces the tier once more at call time (defence in depth).
func tierHook(cur tier.Tier) registry.CallHook {
	return func(_ context.Context, _ *mcp.CallToolRequest, t *registry.Tool, _ any, call func() (string, error)) (string, error) {
		if !cur.Allows(t.MinTier) {
			return "", fmt.Errorf("tool %s requires tier %d, server runs at tier %d", t.Name, t.MinTier, cur)
		}
		return call()
	}
}

// auditMiddleware records every tools/call request, including those the SDK
// rejects before a handler runs (unknown tool, invalid arguments).
func auditMiddleware(a *audit.Logger) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			start := time.Now()
			var name string
			var args json.RawMessage
			if p, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok && p != nil {
				name, args = p.Name, p.Arguments
			}
			session := ""
			if sess := req.GetSession(); sess != nil {
				session = sess.ID()
			}
			res, err := next(ctx, method, req)
			status, callErr := audit.StatusOK, error(nil)
			switch r, _ := res.(*mcp.CallToolResult); {
			case err != nil:
				status, callErr = audit.StatusRejected, err
			case r != nil && r.IsError:
				status, callErr = audit.StatusError, errors.New(firstLine(resultText(r)))
			}
			a.ToolCall(name, session, args, time.Since(start), status, callErr)
			return res, err
		}
	}
}

func resultText(r *mcp.CallToolResult) string {
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			return t.Text
		}
	}
	return ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimPrefix(s, "error: ")
}

type healthInput struct{}

func healthTool(o Options) registry.Tool {
	return registry.NewTool("health_summary", "Health summary",
		"Start here. One call returns OK/WARN/CRIT/UNKNOWN verdicts with reasons for the host and every active module.",
		tier.ReadOnly, true, func(ctx context.Context, _ healthInput) (string, error) {
			checks := append([]registry.Check{tlsCheck(o.Env.Cfg, time.Now())},
				registry.RunHealth(ctx, o.Env, o.Modules, o.Env.Cfg.Limits.HealthTimeout)...)
			return formatHealth(checks), nil
		})
}

// TLS expiry thresholds for health_summary.
const tlsWarnBefore = 30 * 24 * time.Hour

// tlsCheck reports the state of the server's own HTTPS endpoint.
func tlsCheck(cfg *config.Config, now time.Time) registry.Check {
	c := registry.Check{Module: "server", Name: "tls", Status: registry.OK}
	if !cfg.TLS.Enabled() {
		c.Detail = "disabled, plain HTTP on " + cfg.Listen
		if !cfg.ListenIsLoopback() {
			c.Detail += " (network listener, allow_insecure_remote: token travels unencrypted)"
		}
		return c
	}
	p := TLSPaths(cfg)
	info, err := certs.Load(p.Cert)
	if err != nil {
		c.Status, c.Detail = registry.Crit, "cannot read certificate: "+err.Error()
		return c
	}
	kind, _, _ := certs.Classify(p)
	left := info.Remaining(now)
	c.Detail = fmt.Sprintf("certificate valid until %s (%d days)", info.NotAfter.Format("2006-01-02"), info.DaysLeft(now))
	switch kind {
	case certs.Managed:
		c.Detail += ", signed by the single-use local CA (renewal brings a new CA: clients trust it again)"
	case certs.Legacy:
		c.Status = registry.Warn
		c.Detail += "; legacy self-signed certificate, rejected by some clients (native Claude Code): " +
			"run 'omnibusmcp tls generate --force', then trust the CA on clients ('omnibusmcp tls client-setup')"
	case certs.External:
		c.Detail += fmt.Sprintf(", issued by %q; not renewed by OmnibusMCP", info.Issuer)
	}
	switch {
	case left <= 0:
		c.Status, c.Detail = registry.Crit, fmt.Sprintf("certificate EXPIRED on %s; run: omnibusmcp tls renew --force", info.NotAfter.Format("2006-01-02"))
	case left < tlsWarnBefore:
		c.Status = registry.Warn
		c.Detail += "; expires soon"
		if kind == certs.Managed {
			c.Detail += "; omnibusmcp-tls-renew.timer renews it, then run 'omnibusmcp tls client-setup' on clients"
		}
	}
	return c
}

func formatHealth(checks []registry.Check) string {
	var b strings.Builder
	fmt.Fprintf(&b, "OVERALL: %s\n\n", registry.Worst(checks))
	for _, c := range checks {
		fmt.Fprintf(&b, "[%-7s] %s/%s: %s\n", c.Status, c.Module, c.Name, c.Detail)
	}
	return b.String()
}

func instructions(o Options, tools []registry.Tool) string {
	host, _ := os.Hostname()
	var names []string
	for _, m := range o.Modules {
		names = append(names, m.Name())
	}
	mode := "all tools are read-only"
	if o.Env.Cfg.Tier > tier.ReadOnly {
		mode = "some tools can change state (see tool annotations)"
	}
	return fmt.Sprintf("OmnibusMCP diagnostic server on host %q. Tier %s: %s. Active modules: %s. %d tools available. "+
		"Start with health_summary, then drill down with the module tools (prefixed by module name). "+
		"There is no shell access: only the listed tools exist. Command output is size-limited and every command has a timeout; "+
		"a TIMEOUT usually means the queried service is hung.",
		host, o.Env.Cfg.Tier, mode, strings.Join(names, ", "), len(tools))
}

func addPrompts(s *mcp.Server, o Options) {
	s.AddPrompt(&mcp.Prompt{
		Name:        "diagnose_host",
		Title:       "Diagnose this host",
		Description: "Step-by-step diagnosis of the host and its key services.",
		Arguments:   []*mcp.PromptArgument{{Name: "symptom", Description: "optional description of the observed problem"}},
	}, func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		symptom := req.Params.Arguments["symptom"]
		text := "Diagnose this host using the OmnibusMCP tools.\n" +
			"1. Call health_summary and list every WARN/CRIT/UNKNOWN item.\n" +
			"2. For each problem, drill down with the matching module tools (service status, journal with priority=err and a since window, disks, network).\n" +
			"3. Correlate timestamps across logs; check the previous boot (boot=-1) if the host restarted unexpectedly.\n" +
			"4. Report: findings ordered by severity, the evidence for each, the probable root cause and recommended next steps. Do not guess beyond the evidence."
		if symptom != "" {
			text += "\n\nReported symptom: " + symptom
		}
		return &mcp.GetPromptResult{
			Description: "Host diagnosis",
			Messages:    []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: text}}},
		}, nil
	})
}

func addResources(s *mcp.Server, o Options, tools []registry.Tool) {
	s.AddResource(&mcp.Resource{
		URI: "omnibus://server/info", Name: "server-info", MIMEType: "application/json",
		Description: "Server version, tier, active modules, exposed tools and compatibility of the detected product versions.",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		type toolInfo struct {
			Name     string `json:"name"`
			Module   string `json:"module"`
			MinTier  int    `json:"min_tier"`
			ReadOnly bool   `json:"read_only"`
		}
		info := struct {
			Version string     `json:"version"`
			Tier    int        `json:"tier"`
			Modules []string   `json:"modules"`
			Tools   []toolInfo `json:"tools"`
			// Compatibility of the products the modules depend on; untested
			// versions work but were not verified (see README).
			Compatibility []compat.Result `json:"compatibility"`
		}{Version: o.Version, Tier: int(o.Env.Cfg.Tier)}
		for _, m := range o.Modules {
			info.Modules = append(info.Modules, m.Name())
			if v, ok := m.(registry.Versioned); ok {
				info.Compatibility = append(info.Compatibility, v.Version(ctx, o.Env))
			}
		}
		for _, t := range tools {
			mod := t.Module
			if mod == "" {
				mod = "server"
			}
			info.Tools = append(info.Tools, toolInfo{t.Name, mod, int(t.MinTier), t.ReadOnly})
		}
		data, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(data)}}}, nil
	})
}

// ReadToken loads the Bearer token from file.
func ReadToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read token: %w", err)
	}
	tok := strings.TrimSpace(string(data))
	if len(tok) < 32 {
		return "", fmt.Errorf("token in %s is too short (min 32 characters)", path)
	}
	return tok, nil
}

// requireBearer rejects requests without the exact token.
func requireBearer(token string, a *audit.Logger, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := bearerToken(r.Header.Get("Authorization"))
		gotSum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(want[:], gotSum[:]) != 1 {
			reason := "invalid token"
			if !ok {
				reason = "missing bearer token"
			}
			a.AuthFailure(r.RemoteAddr, reason)
			w.Header().Set("WWW-Authenticate", `Bearer realm="omnibusmcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// bearerToken extracts the token; the scheme is case-insensitive (RFC 7235).
func bearerToken(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// Handler returns the HTTP handler serving MCP at /mcp.
// Public configures the pages served without a token over HTTPS; nil
// serves none (plain HTTP or tests).
type Public struct {
	CAFile    string // local CA certificate, served at CAPath
	TokenFile string // shown on the landing page as where the token lives
	Listen    string // fallback host and port for the landing page's URLs
}

func Handler(s *mcp.Server, token string, a *audit.Logger, pub *Public) http.Handler {
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s },
		&mcp.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute, MaxRequestBodyBytes: 1 << 20})
	mux := http.NewServeMux()
	mux.Handle("/mcp", requireBearer(token, a, mcpHandler))
	if pub != nil {
		mux.HandleFunc("GET "+CAPath, caHandler(pub.CAFile))
		mux.HandleFunc("GET /{$}", landingHandler(pub))
	}
	return mux
}

// CAPath is where clients download the local CA certificate. It needs no
// token: the certificate is public (the server sends it in every TLS
// handshake). Clients trust it on first use.
const CAPath = clientsetup.CAPath

// landingHandler serves instructions for new clients at "/": what this
// endpoint is and how to trust its CA. The URLs use the host the client
// asked for, so they match what the client can reach.
func landingHandler(pub *Public) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t := clientsetup.Target{}
		t.Host, t.Port, _ = net.SplitHostPort(pub.Listen)
		if h, p, err := net.SplitHostPort(r.Host); err == nil && validHost.MatchString(h) {
			t.Host, t.Port = h, p
		} else if validHost.MatchString(r.Host) {
			t.Host = r.Host
		}
		_, err := certs.ReadCA(pub.CAFile) // only an OmnibusMCP CA is offered
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(clientsetup.LandingPage(t, err == nil, pub.TokenFile)))
	}
}

// validHost accepts DNS names and IP addresses (IPv6 without brackets, as
// net.SplitHostPort returns them); anything else in the Host header is
// ignored rather than echoed.
var validHost = regexp.MustCompile(`^[A-Za-z0-9.:-]{1,253}$`)

// caHandler serves the local CA. The file is read per request so a new CA
// is served without a restart; only a CA created by OmnibusMCP is served.
func caHandler(caFile string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		data, err := certs.ReadCA(caFile)
		if err != nil {
			http.Error(w, "no OmnibusMCP CA on this server", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(data)
	}
}

// TLSPaths returns the server certificate, key and local CA files.
func TLSPaths(cfg *config.Config) certs.Paths {
	return certs.Paths{Cert: cfg.TLS.CertFile, Key: cfg.TLS.KeyFile, CA: cfg.TLS.CAFile()}
}

// LoadTLS loads the configured certificate and key, or returns nil when TLS
// is disabled. Loading happens before listening, so a missing or broken
// certificate is reported as a configuration error.
func LoadTLS(cfg *config.Config) (*tls.Config, error) {
	if !cfg.TLS.Enabled() {
		return nil, nil
	}
	pair, err := tls.LoadX509KeyPair(cfg.TLS.CertFile, cfg.TLS.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}

// Serve runs the HTTP server until ctx is cancelled; tlsConf nil means
// plain HTTP.
func Serve(ctx context.Context, cfg *config.Config, tlsConf *tls.Config, h http.Handler) error {
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		TLSConfig:         tlsConf,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	errc := make(chan error, 1)
	go func() {
		if tlsConf != nil {
			errc <- srv.ServeTLS(ln, "", "")
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return err
		}
		if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
