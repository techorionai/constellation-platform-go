package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/techorionai/constellation-platform-go/auth"
)

const (
	// DefaultMaxBodyBytes is the default of [Config].MaxBodyBytes.
	DefaultMaxBodyBytes int64 = 1 << 20

	metadataPrefix = "/.well-known/oauth-protected-resource"
)

var appKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Config is the configuration of a [Server].
type Config struct {
	// App is the key of the application, for example "helpdesk". The scopes of
	// each tool start with App and a colon.
	App string
	// Name and Version are the server info that a client reads.
	Name, Version string
	// Verifier checks the bearer token of each request.
	Verifier auth.Verifier
	// ResourceURL is the public URL of the MCP endpoint.
	ResourceURL string
	// AuthorizationServers are the issuers of the tokens. The metadata lists them.
	AuthorizationServers []string
	// AllowedOrigins are the values of the Origin header that the server
	// accepts. A request with no Origin header is always accepted.
	AllowedOrigins []string
	// MaxBodyBytes is the maximum size of a request body. The default is 1 MiB.
	MaxBodyBytes int64
	// OnRefused runs one time for each tool call that a missing scope refuses.
	OnRefused func(ctx context.Context, r Refusal)
	// OnInvalid runs one time for each tool call with bad arguments.
	OnInvalid func(ctx context.Context, r Refusal)
	// MeterProvider is the meter provider. The default is the global one.
	MeterProvider metric.MeterProvider
	// TracerProvider is the tracer provider. The default is the global one.
	TracerProvider trace.TracerProvider
}

// Refusal is the data of a refused tool call. A hook must not change Arguments.
type Refusal struct {
	Tool string
	// Missing are the scopes that the principal does not have. It is empty for
	// [Config].OnInvalid.
	Missing []string
	// Arguments are the arguments of the call, as the client sent them.
	Arguments json.RawMessage
}

// Server is an MCP server with scoped tools.
type Server struct {
	cfg         Config
	mcp         *mcp.Server
	tel         telemetry
	metadataURL string

	mu         sync.RWMutex
	tools      map[string][]string // tool name -> scopes
	linksAdded bool
}

// New returns a server. It returns an error for a bad config.
func New(cfg Config) (*Server, error) {
	if !appKeyPattern.MatchString(cfg.App) {
		return nil, fmt.Errorf("mcpserver: App %q is not a valid app key", cfg.App)
	}
	if cfg.Name == "" {
		return nil, errors.New("mcpserver: Name is empty")
	}
	if cfg.Verifier == nil {
		return nil, errors.New("mcpserver: Verifier is nil")
	}
	u, err := url.Parse(cfg.ResourceURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("mcpserver: ResourceURL is not an absolute http or https URL")
	}
	if len(cfg.AuthorizationServers) == 0 {
		return nil, errors.New("mcpserver: AuthorizationServers is empty")
	}
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if cfg.MeterProvider == nil {
		cfg.MeterProvider = otel.GetMeterProvider()
	}
	if cfg.TracerProvider == nil {
		cfg.TracerProvider = otel.GetTracerProvider()
	}
	path := u.EscapedPath()
	if path == "/" {
		path = ""
	}
	s := &Server{
		cfg:         cfg,
		mcp:         mcp.NewServer(&mcp.Implementation{Name: cfg.Name, Version: cfg.Version}, nil),
		tel:         newTelemetry(cfg.MeterProvider, cfg.TracerProvider),
		metadataURL: u.Scheme + "://" + u.Host + metadataPrefix + path,
		tools:       map[string][]string{},
	}
	return s, nil
}

// scopesOf returns the scopes of a known tool.
func (s *Server) scopesOf(tool string) ([]string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sc, ok := s.tools[tool]
	return sc, ok
}

func (s *Server) allScopes() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := map[string]struct{}{}
	for _, sc := range s.tools {
		for _, v := range sc {
			set[v] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
