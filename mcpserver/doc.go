// Package mcpserver gives an application a built-in MCP server. Each tool
// declares the scopes that a caller needs. The package checks the token, the
// scopes, and the arguments of each tool call.
//
// The transport is Streamable HTTP in stateless mode with JSON responses. The
// token check uses the [auth.Verifier] of the service. A missing scope gives
// HTTP 403 with the problem body of [auth.WriteInsufficientScope]. The package
// serves the protected resource metadata of RFC 9728 and the link templates
// resource of the application.
//
//	s, _ := mcpserver.New(mcpserver.Config{
//	    App: "helpdesk", Name: "helpdesk", Version: "1.0.0",
//	    Verifier: verifier, ResourceURL: "https://helpdesk.example.com/mcp",
//	    AuthorizationServers: []string{"https://identity.example.com"},
//	})
//	_ = mcpserver.AddTool(s, mcpserver.ToolSpec{
//	    Name: "get_ticket", Description: "Get one ticket.",
//	    Scopes: []string{"helpdesk:ticket:read"}, ReadOnly: true,
//	}, getTicket)
//	mux.Handle("/mcp", s.Handler())
//	mux.Handle("/.well-known/oauth-protected-resource/mcp", s.MetadataHandler())
//
// Each response of [Server.Handler] has the headers Cache-Control: no-store and
// Pragma: no-cache. The package never writes the arguments or the results of a
// tool call to a log or a span.
package mcpserver
