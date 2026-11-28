package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// LinkTemplate maps a resource type to a URL template. It follows the model
// LinkTemplate of constellation-contracts.
type LinkTemplate struct {
	ResourceType string `json:"resourceType"`
	Template     string `json:"template"`
	Description  string `json:"description,omitempty"`
}

// LinkTemplateSet is the link templates of one application. It follows the model
// LinkTemplateSet of constellation-contracts.
type LinkTemplateSet struct {
	App       string         `json:"app"`
	BaseURL   string         `json:"baseUrl"`
	Templates []LinkTemplate `json:"templates"`
}

var (
	resourceTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	templatePattern     = regexp.MustCompile(`^\{base_url\}/[A-Za-z0-9._~/%-]*(\{[a-z_][a-z0-9_]*\}[A-Za-z0-9._~/%-]*)*$`)
	baseURLPattern      = regexp.MustCompile(`^(https://[A-Za-z0-9.-]+|http://localhost)(:[0-9]{1,5})?(/[A-Za-z0-9._~%-]+)*$`)
)

// Validate checks the set with the rules of lib/links.tsp of
// constellation-contracts. It checks the app against the given app key.
func (set LinkTemplateSet) Validate(app string) error {
	if set.App != app {
		return fmt.Errorf("mcpserver: link templates app %q is not %q", set.App, app)
	}
	if !baseURLPattern.MatchString(set.BaseURL) {
		return errors.New("mcpserver: link templates baseUrl is not valid")
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(set.BaseURL, "https://"), "http://")
	if _, path, found := strings.Cut(rest, "/"); found {
		for seg := range strings.SplitSeq(path, "/") {
			if seg == "." || seg == ".." {
				return errors.New("mcpserver: link templates baseUrl has a dot segment")
			}
		}
	}
	if len(set.Templates) == 0 {
		return errors.New("mcpserver: link templates list is empty")
	}
	seen := map[string]struct{}{}
	for _, t := range set.Templates {
		if !resourceTypePattern.MatchString(t.ResourceType) || len(t.ResourceType) > 64 {
			return fmt.Errorf("mcpserver: resourceType %q is not valid", t.ResourceType)
		}
		if _, dup := seen[t.ResourceType]; dup {
			return fmt.Errorf("mcpserver: resourceType %q occurs two times", t.ResourceType)
		}
		seen[t.ResourceType] = struct{}{}
		if strings.Contains(t.Template, "..") || !templatePattern.MatchString(t.Template) {
			return fmt.Errorf("mcpserver: template of resourceType %q is not valid", t.ResourceType)
		}
	}
	return nil
}

// AddLinkTemplates adds the resource constellation://<app>/link-templates. A
// read of the resource needs a valid token and no scope. It returns an error for
// a bad set, or if the server has the resource already.
func (s *Server) AddLinkTemplates(set LinkTemplateSet) error {
	if err := set.Validate(s.cfg.App); err != nil {
		return err
	}
	body, err := json.Marshal(set)
	if err != nil {
		return fmt.Errorf("mcpserver: marshal link templates: %w", err)
	}
	s.mu.Lock()
	if s.linksAdded {
		s.mu.Unlock()
		return errors.New("mcpserver: link templates are already added")
	}
	s.linksAdded = true
	s.mu.Unlock()

	uri := "constellation://" + s.cfg.App + "/link-templates"
	s.mcp.AddResource(&mcp.Resource{
		URI: uri, Name: "link-templates", Title: "Link templates",
		Description: "Maps each resource type of the application to a URL in its UI.",
		MIMEType:    "application/json",
	}, func(_ context.Context, _ *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
			{URI: uri, MIMEType: "application/json", Text: string(body)},
		}}, nil
	})
	return nil
}
