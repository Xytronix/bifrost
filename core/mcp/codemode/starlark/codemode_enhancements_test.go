//go:build !tinygo && !wasm

package starlark

import (
	"context"
	"strings"
	"testing"
	"time"

	codemcp "github.com/maximhq/bifrost/core/mcp"
	"github.com/maximhq/bifrost/core/schemas"
)

// twoServerManager returns a test client manager with two code-mode servers
// ("alpha" exposing tool "ping", "beta" exposing tool "pong").
func twoServerManager() *testClientManager {
	mk := func(name string) *schemas.MCPClientState {
		return &schemas.MCPClientState{
			Name:            name,
			ExecutionConfig: &schemas.MCPClientConfig{Name: name, IsCodeModeClient: true},
		}
	}
	tool := func(name string) schemas.ChatTool {
		return schemas.ChatTool{
			Function: &schemas.ChatToolFunction{
				Name: name,
				Parameters: &schemas.ToolFunctionParameters{
					Type:       "object",
					Properties: schemas.NewOrderedMap(),
					Required:   []string{},
				},
			},
		}
	}
	return &testClientManager{
		clients: map[string]*schemas.MCPClientState{
			"alpha": mk("alpha"),
			"beta":  mk("beta"),
		},
		tools: map[string][]schemas.ChatTool{
			"alpha": {tool("ping")},
			"beta":  {tool("pong")},
		},
	}
}

func toolCallArgs(name, args string) schemas.ChatAssistantMessageToolCall {
	return schemas.ChatAssistantMessageToolCall{
		ID: schemas.Ptr("tc"),
		Function: schemas.ChatAssistantMessageToolCallFunction{
			Name:      schemas.Ptr(name),
			Arguments: args,
		},
	}
}

func toolResponseContent(t *testing.T, msg *schemas.ChatMessage) string {
	t.Helper()
	if msg == nil || msg.Content == nil || msg.Content.ContentStr == nil {
		t.Fatal("expected tool response content")
	}
	return *msg.Content.ContentStr
}

// TestListToolFilesServerFilter covers issue #4434 (item C): listToolFiles can
// restrict the listing to a single server, and reports clearly when the filter
// matches nothing.
func TestListToolFilesServerFilter(t *testing.T) {
	mode := NewStarlarkCodeMode(&codemcp.CodeModeConfig{
		BindingLevel:         schemas.CodeModeBindingLevelServer,
		ToolExecutionTimeout: time.Second,
	}, nil)
	mode.clientManager = twoServerManager()

	msg, err := mode.handleListToolFiles(context.Background(), toolCallArgs("listToolFiles", `{"server":"alpha"}`))
	if err != nil {
		t.Fatalf("handleListToolFiles error: %v", err)
	}
	filtered := toolResponseContent(t, msg)
	if !strings.Contains(filtered, "alpha.pyi") {
		t.Fatalf("expected alpha.pyi in filtered response, got:\n%s", filtered)
	}
	if strings.Contains(filtered, "beta.pyi") {
		t.Fatalf("did not expect beta.pyi in alpha-filtered response, got:\n%s", filtered)
	}

	msgAll, err := mode.handleListToolFiles(context.Background(), toolCallArgs("listToolFiles", ""))
	if err != nil {
		t.Fatalf("handleListToolFiles (no filter) error: %v", err)
	}
	all := toolResponseContent(t, msgAll)
	if !strings.Contains(all, "alpha.pyi") || !strings.Contains(all, "beta.pyi") {
		t.Fatalf("expected both servers without filter, got:\n%s", all)
	}

	msgNone, err := mode.handleListToolFiles(context.Background(), toolCallArgs("listToolFiles", `{"server":"ghost"}`))
	if err != nil {
		t.Fatalf("handleListToolFiles (unknown) error: %v", err)
	}
	none := toolResponseContent(t, msgNone)
	if !strings.Contains(none, "ghost") {
		t.Fatalf("expected filter-specific message mentioning 'ghost', got:\n%s", none)
	}
}

// TestReadToolFileBatch covers issue #4434 (item D): readToolFile reads multiple
// stub files in one call via fileNames.
func TestReadToolFileBatch(t *testing.T) {
	mode := NewStarlarkCodeMode(&codemcp.CodeModeConfig{
		BindingLevel:         schemas.CodeModeBindingLevelServer,
		ToolExecutionTimeout: time.Second,
	}, nil)
	mode.clientManager = twoServerManager()

	msg, err := mode.handleReadToolFile(context.Background(), toolCallArgs("readToolFile", `{"fileNames":["servers/alpha.pyi","servers/beta.pyi"]}`))
	if err != nil {
		t.Fatalf("handleReadToolFile batch error: %v", err)
	}
	content := toolResponseContent(t, msg)
	if !strings.Contains(content, "ping") {
		t.Fatalf("expected alpha tool 'ping' in batch response, got:\n%s", content)
	}
	if !strings.Contains(content, "pong") {
		t.Fatalf("expected beta tool 'pong' in batch response, got:\n%s", content)
	}
}

// TestExecuteToolCodeOmitEnvironmentFooter covers issue #4434 (item A): the
// trailing environment block is appended by default but suppressed when
// OmitEnvironmentFooter is set.
func TestExecuteToolCodeOmitEnvironmentFooter(t *testing.T) {
	ctx := schemas.NewBifrostContext(context.Background(), schemas.NoDeadline)
	call := toolCallArgs("executeToolCode", `{"code":"result = 2"}`)

	def := NewStarlarkCodeMode(&codemcp.CodeModeConfig{
		BindingLevel:         schemas.CodeModeBindingLevelServer,
		ToolExecutionTimeout: time.Second,
	}, nil)
	def.clientManager = twoServerManager()
	msgDef, err := def.handleExecuteToolCode(ctx, call)
	if err != nil {
		t.Fatalf("handleExecuteToolCode (default) error: %v", err)
	}
	defContent := toolResponseContent(t, msgDef)
	if !strings.Contains(defContent, "Available server keys") {
		t.Fatalf("expected environment footer by default, got:\n%s", defContent)
	}

	omit := NewStarlarkCodeMode(&codemcp.CodeModeConfig{
		BindingLevel:          schemas.CodeModeBindingLevelServer,
		ToolExecutionTimeout:  time.Second,
		OmitEnvironmentFooter: true,
	}, nil)
	omit.clientManager = twoServerManager()
	msgOmit, err := omit.handleExecuteToolCode(ctx, call)
	if err != nil {
		t.Fatalf("handleExecuteToolCode (omit) error: %v", err)
	}
	omitContent := toolResponseContent(t, msgOmit)
	if strings.Contains(omitContent, "Available server keys") {
		t.Fatalf("expected no environment footer when omitted, got:\n%s", omitContent)
	}
	if strings.Contains(omitContent, "Note: This is a Starlark") {
		t.Fatalf("expected no Starlark note when omitted, got:\n%s", omitContent)
	}
	if !strings.Contains(omitContent, "Return value") {
		t.Fatalf("expected the result to still be returned, got:\n%s", omitContent)
	}
}
