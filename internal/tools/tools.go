// Package tools exposes mxread operations as MCP tools. It is a thin adapter
// over internal/app so the MCP SDK can be swapped easily.
package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/RoiArthurB/mxread/internal/app"
	"github.com/RoiArthurB/mxread/internal/matrix"
	"github.com/RoiArthurB/mxread/internal/render"
	"github.com/RoiArthurB/mxread/internal/resolve"
)

// Inputs. Descriptions are one line each: every schema is sent with every
// LLM call (SPEC §5).

// RecentIn is the input of the recent tool.
type RecentIn struct {
	Since   string `json:"since,omitempty" jsonschema:"ISO time or relative (24h, 3d). Default 24h"`
	Limit   int    `json:"limit,omitempty" jsonschema:"Max total messages, default 40, max 200"`
	PerChat int    `json:"per_chat,omitempty" jsonschema:"Max messages per chat, default 10"`
	Network string `json:"network,omitempty" jsonschema:"Only this network, e.g. whatsapp"`
	Chat    string `json:"chat,omitempty" jsonschema:"Only this chat (name or room ID)"`
}

// ChatsIn is the input of the chats tool.
type ChatsIn struct {
	Query        string `json:"query,omitempty" jsonschema:"Filter by chat name"`
	ActiveWithin string `json:"active_within,omitempty" jsonschema:"Only chats active within e.g. 7d"`
	Network      string `json:"network,omitempty" jsonschema:"Only this network, e.g. whatsapp"`
	Limit        int    `json:"limit,omitempty" jsonschema:"Max chats, default 30"`
}

// ReadIn is the input of the read tool.
type ReadIn struct {
	Chat   string `json:"chat" jsonschema:"Chat name or room ID"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Max messages, default 30, max 100"`
	Before string `json:"before,omitempty" jsonschema:"Only messages before this ISO/relative time"`
	After  string `json:"after,omitempty" jsonschema:"Only messages after this ISO/relative time"`
	Cursor string `json:"cursor,omitempty" jsonschema:"Cursor from a previous read, for older messages"`
}

// SearchIn is the input of the search tool.
type SearchIn struct {
	Query string `json:"query" jsonschema:"Words to find"`
	Chat  string `json:"chat,omitempty" jsonschema:"Only this chat (name or room ID)"`
	Since string `json:"since,omitempty" jsonschema:"ISO time or relative (30d)"`
	Limit int    `json:"limit,omitempty" jsonschema:"Max results, default 15"`
}

// ChatInfoIn is the input of the chat_info tool.
type ChatInfoIn struct {
	Chat string `json:"chat" jsonschema:"Chat name or room ID"`
}

var no = false

// lenient infers the input schema for T but accepts unknown properties: small
// models sometimes add extra arguments, and a retry costs seconds. Dropping
// "additionalProperties": false also saves schema tokens.
func lenient[T any]() *jsonschema.Schema {
	s, err := jsonschema.For[T](nil)
	if err != nil {
		panic(err)
	}
	s.AdditionalProperties = nil
	return s
}

func annotations() *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &no, IdempotentHint: true}
}

// Register adds the five tools to s.
func Register(s *mcp.Server, a *app.App) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "recent",
		InputSchema: lenient[RecentIn](),
		Description: "Latest messages across all chats, grouped by chat.\nUse for: what are my latest messages, what did I miss, daily summary.",
		Annotations: annotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in RecentIn) (*mcp.CallToolResult, any, error) {
		r, err := a.Recent(ctx, app.RecentParams{Since: in.Since, Limit: in.Limit, PerChat: in.PerChat, Network: in.Network, Chat: in.Chat})
		return finish(a, r, err, func() string { return render.Recent(r, a.Clock()) })
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "chats",
		InputSchema: lenient[ChatsIn](),
		Description: "List chats (name, network, DM/group, last activity, room ID), most recent first.\nUse to find a chat or see which chats are active.",
		Annotations: annotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ChatsIn) (*mcp.CallToolResult, any, error) {
		r, err := a.Chats(ctx, app.ChatsParams{Query: in.Query, ActiveWithin: in.ActiveWithin, Network: in.Network, Limit: in.Limit})
		return finish(a, r, err, func() string { return render.Chats(r, a.Clock()) })
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "read",
		InputSchema: lenient[ReadIn](),
		Description: "Read one chat in detail, oldest to newest, with a cursor for older messages.\nUse to read a conversation.",
		Annotations: annotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ReadIn) (*mcp.CallToolResult, any, error) {
		r, err := a.Read(ctx, app.ReadParams{Chat: in.Chat, Limit: in.Limit, Before: in.Before, After: in.After, Cursor: in.Cursor})
		return finish(a, r, err, func() string { return render.Read(r, a.Clock()) })
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "search",
		InputSchema: lenient[SearchIn](),
		Description: "Find messages containing words, with chat, time and sender.\nUse to find who said what, when.",
		Annotations: annotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SearchIn) (*mcp.CallToolResult, any, error) {
		r, err := a.Search(ctx, app.SearchParams{Query: in.Query, Chat: in.Chat, Since: in.Since, Limit: in.Limit})
		return finish(a, r, err, func() string { return render.Search(r, a.Clock()) })
	})
	mcp.AddTool(s, &mcp.Tool{
		Name:        "chat_info",
		InputSchema: lenient[ChatInfoIn](),
		Description: "Chat details: name, topic, network, DM/group, members.\nUse for who is in a chat.",
		Annotations: annotations(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in ChatInfoIn) (*mcp.CallToolResult, any, error) {
		r, err := a.ChatInfo(ctx, in.Chat)
		return finish(a, r, err, func() string { return render.ChatInfo(r, a.Clock()) })
	})
}

// finish builds the tool result: text for the model, structured JSON for
// clients that use it. Errors become IsError results with a short message.
func finish[T any](a *app.App, r *T, err error, text func() string) (*mcp.CallToolResult, any, error) {
	defer a.Close()
	if err != nil {
		var amb *resolve.AmbiguousError
		if errors.As(err, &amb) {
			// Not a failure: the model should pick one of the candidates.
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: amb.Error()}}}, nil, nil
		}
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: errorText(err)}}}, nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text()}}}, r, nil
}

func errorText(err error) string {
	switch {
	case app.IsUserError(err):
		return err.Error()
	case matrix.IsAuth(err):
		return "Matrix login failed (access token rejected). Tell the user to check MATRIX_ACCESS_TOKEN."
	case errors.Is(err, context.DeadlineExceeded):
		return "Matrix server timed out. Try again later or narrow the request."
	default:
		return fmt.Sprintf("Matrix request failed: %v", err)
	}
}
