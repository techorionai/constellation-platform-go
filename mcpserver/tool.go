package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/techorionai/constellation-platform-go/auth"
)

// ScopesMetaKey is the key of the required scopes in the _meta of a tool.
const ScopesMetaKey = "ai.techorion.constellation/requiredScopes"

// ToolSpec describes a tool.
type ToolSpec struct {
	Name, Title, Description string
	// Scopes are the permission keys of the own app. A caller needs all of them.
	Scopes               []string
	ReadOnly, Idempotent bool
	// InputSchema replaces the schema that the package infers from the input
	// type. Use it for enum, maxLength, and format.
	InputSchema *jsonschema.Schema
	// OutputSchema replaces the schema that the package infers from the output
	// type.
	OutputSchema *jsonschema.Schema
}

// AddTool registers a tool. The handler gets the principal and the typed
// input. The package checks the scopes and the arguments before it calls the
// handler. AddTool returns an error for a bad spec and does not panic.
func AddTool[In, Out any](s *Server, spec ToolSpec, h func(ctx context.Context, p auth.Principal, in In) (Out, error)) error {
	if err := s.checkSpec(spec); err != nil {
		return err
	}
	inSchema := spec.InputSchema
	if inSchema == nil {
		var err error
		if inSchema, err = jsonschema.For[In](nil); err != nil {
			return fmt.Errorf("mcpserver: tool %q: input schema: %w", spec.Name, err)
		}
	}
	if inSchema.Type != "object" {
		return fmt.Errorf(`mcpserver: tool %q: input schema must have type "object"`, spec.Name)
	}
	inResolved, err := inSchema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("mcpserver: tool %q: input schema: %w", spec.Name, err)
	}
	outSchema := spec.OutputSchema
	if outSchema == nil && reflect.TypeFor[Out]() != reflect.TypeFor[any]() {
		if outSchema, err = jsonschema.For[Out](nil); err != nil {
			return fmt.Errorf("mcpserver: tool %q: output schema: %w", spec.Name, err)
		}
	}
	tool := &mcp.Tool{
		Name:        spec.Name,
		Title:       spec.Title,
		Description: spec.Description,
		InputSchema: inSchema,
		Meta:        mcp.Meta{ScopesMetaKey: append([]string(nil), spec.Scopes...)},
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: spec.ReadOnly, IdempotentHint: spec.Idempotent},
	}
	if outSchema != nil {
		if _, err := outSchema.Resolve(nil); err != nil {
			return fmt.Errorf("mcpserver: tool %q: output schema: %w", spec.Name, err)
		}
		tool.OutputSchema = outSchema
	}
	if spec.ReadOnly {
		f := false
		tool.Annotations.DestructiveHint = &f
	}
	scopes := append([]string(nil), spec.Scopes...)

	s.mu.Lock()
	if _, dup := s.tools[spec.Name]; dup {
		s.mu.Unlock()
		return fmt.Errorf("mcpserver: tool %q is already registered", spec.Name)
	}
	s.tools[spec.Name] = scopes
	s.mu.Unlock()

	s.mcp.AddTool(tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return callTool(ctx, s, spec.Name, scopes, inResolved, req.Params.Arguments, h), nil
	})
	return nil
}

func (s *Server) checkSpec(spec ToolSpec) error {
	if spec.Name == "" {
		return errors.New("mcpserver: tool name is empty")
	}
	if len(spec.Scopes) == 0 {
		return fmt.Errorf("mcpserver: tool %q has no scope", spec.Name)
	}
	for _, sc := range spec.Scopes {
		if !strings.HasPrefix(sc, s.cfg.App+":") || len(sc) == len(s.cfg.App)+1 {
			return fmt.Errorf("mcpserver: tool %q: scope %q is not a scope of app %q", spec.Name, sc, s.cfg.App)
		}
	}
	s.mu.RLock()
	_, dup := s.tools[spec.Name]
	s.mu.RUnlock()
	if dup {
		return fmt.Errorf("mcpserver: tool %q is already registered", spec.Name)
	}
	return nil
}

func callTool[In, Out any](ctx context.Context, s *Server, name string, scopes []string, schema *jsonschema.Resolved,
	args json.RawMessage, h func(context.Context, auth.Principal, In) (Out, error)) *mcp.CallToolResult {
	ctx, span := s.tel.start(ctx, name)
	defer span.End()

	p, ok := auth.PrincipalFromContext(ctx)
	missing := scopes
	if ok {
		missing = p.MissingScopes(scopes...)
	}
	if len(missing) > 0 {
		if s.cfg.OnRefused != nil {
			s.cfg.OnRefused(ctx, Refusal{Tool: name, Missing: missing, Arguments: args})
		}
		s.tel.count(ctx, name, outcomeDenied)
		return toolErrorResult(&ToolError{Code: "insufficient_scope", Message: "missing scopes: " + strings.Join(missing, " ")})
	}

	value, fields := checkArguments(schema, args)
	if len(fields) > 0 {
		if s.cfg.OnInvalid != nil {
			s.cfg.OnInvalid(ctx, Refusal{Tool: name, Arguments: args})
		}
		s.tel.count(ctx, name, outcomeInvalid)
		return toolErrorResult(&ToolError{Code: "invalid_arguments", Message: "the arguments are not valid", Fields: fields})
	}
	var in In
	if err := json.Unmarshal(value, &in); err != nil {
		// The schema allowed a value that the type does not accept.
		if s.cfg.OnInvalid != nil {
			s.cfg.OnInvalid(ctx, Refusal{Tool: name, Arguments: args})
		}
		s.tel.count(ctx, name, outcomeInvalid)
		return toolErrorResult(&ToolError{Code: "invalid_arguments", Message: "the arguments are not valid",
			Fields: []FieldError{{Field: "", Code: "invalid", Message: "the arguments do not match the input type"}}})
	}

	out, err := h(ctx, p, in)
	if err != nil {
		s.tel.count(ctx, name, outcomeError)
		var te *ToolError
		if errors.As(err, &te) {
			return toolErrorResult(te)
		}
		slog.ErrorContext(ctx, "mcpserver: tool handler failed", slog.String("tool", name), slog.Any("error", err))
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: internalErrorJSON}}}
	}
	body, err := json.Marshal(out)
	if err != nil {
		slog.ErrorContext(ctx, "mcpserver: tool result is not valid JSON", slog.String("tool", name), slog.Any("error", err))
		s.tel.count(ctx, name, outcomeError)
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: internalErrorJSON}}}
	}
	s.tel.count(ctx, name, outcomeSuccess)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(body)}},
		StructuredContent: json.RawMessage(body),
	}
}

func toolErrorResult(e *ToolError) *mcp.CallToolResult {
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: e.json()}}}
}
