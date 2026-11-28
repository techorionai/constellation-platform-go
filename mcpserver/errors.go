package mcpserver

import "encoding/json"

// FieldError is one bad value of the arguments of a tool call.
type FieldError struct {
	// Field is a JSON Pointer (RFC 6901) to the bad value in the arguments.
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ToolError is an error that a tool handler returns for the caller. The package
// turns it into a tool result with isError true. The text of the result is the
// JSON of the ToolError.
type ToolError struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Fields  []FieldError `json:"fields,omitempty"`
}

// Error returns the code and the message.
func (e *ToolError) Error() string { return e.Code + ": " + e.Message }

func (e *ToolError) json() string {
	b, err := json.Marshal(e)
	if err != nil {
		return `{"code":"internal","message":"internal error"}`
	}
	return string(b)
}

const internalErrorJSON = `{"code":"internal","message":"internal error"}`
