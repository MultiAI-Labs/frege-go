# frege-go

A small, dependency-free Go client for the [Frege](https://frege.io) HTTP API.

Frege turns each project's OpenAPI spec into a set of tools and runs them against
the upstream API with the right credential injected. This client lets your own
code call those tools directly — a Telegram bot, a cron job, a backend service —
with no AI model in the loop.

```go
res, err := client.InvokeTool(ctx, projectID, "get_account_profile", nil)
fmt.Println(res.StatusCode, res.Body) // the upstream's own response
```

The whole package is one file and the standard library. Nothing else.

## Install

```bash
go get github.com/MultiAI-Labs/frege-go
```

## Quickest start: an API key

Mint a key in the dashboard: **Project → API keys**. It is shown once, starts
with `frege_sk_`, and belongs to that one project. There is no sign-in and no
token to refresh — a key is a static credential your code holds:

```go
package main

import (
	"context"
	"fmt"
	"os"

	frege "github.com/MultiAI-Labs/frege-go"
)

func main() {
	client := frege.New(frege.StaticToken(os.Getenv("FREGE_API_KEY")))

	res, err := client.InvokeTool(context.Background(), 3, "get_account_profile", nil)
	if err != nil {
		panic(err)
	}
	fmt.Println("upstream said", res.StatusCode)
	fmt.Println(res.Body)
}
```

That is the whole setup. A key can **list** a project's tools and **invoke**
them, bounded by the project's write policy, and nothing else — it cannot reach
project settings or any other project. Keep it secret like any password; anyone
holding it can run that project's tools. Revoke it in the dashboard if it leaks.

`InvokeTool` returns the **upstream's own** response. `res.StatusCode` and
`res.Body` are what the third-party API answered. A `404` there means the
upstream returned 404 — not that the tool is missing. A missing tool, a bad
argument, or an auth failure comes back as an `*APIError` instead (see below).

Arguments are keyed by the tool's input schema:

```go
res, err := client.InvokeTool(ctx, 3, "add_favorite", map[string]any{
	"stock_id": 42,
})
```

## Discover a project's tools

You need the numeric **project id** (it is in the dashboard URL). Then:

```go
ops, err := client.ListOperations(ctx, 3)
for _, op := range ops {
	fmt.Printf("%s  %s %s — %s\n", op.ToolName, op.Method, op.Path, op.Summary)
	// op.InputSchema is the JSON Schema for its arguments.
}
```

## Errors

Failures from Frege itself are a typed `*APIError`:

```go
res, err := client.InvokeTool(ctx, 3, "add_favorite", args)
if err != nil {
	var apiErr *frege.APIError
	if errors.As(err, &apiErr) {
		fmt.Println(apiErr.StatusCode, apiErr.Code, apiErr.Message)
		fmt.Println("request id:", apiErr.RequestID) // quote this to support
		if apiErr.StatusCode == 422 {
			fmt.Println("bad arguments:", apiErr.Fields)
		}
	}
}
```

## Environments

| Environment | Base URL |
|---|---|
| Production | `https://frege.io` (default) |
| Dev | `https://frege.uz` |

```go
client := frege.New(frege.StaticToken(key), frege.WithBaseURL("https://frege.uz"))
```

## Alternative: act as a user (no API key)

If you have no API key — or you specifically want to act as a **person** rather
than a project credential — sign in once with an email code and let the client
keep the session alive. This is heavier: it needs a one-time human step and a
rotating refresh token.

Get a refresh token once:

```bash
go run ./examples/bootstrap
# Email: you@yourcompany.com  → (check inbox) → Code: 123456
# prints a refresh token to store
```

Then use it:

```go
tok := frege.NewRefreshingToken("", os.Getenv("FREGE_REFRESH_TOKEN"),
	frege.WithOnRefresh(func(_, refresh string) {
		_ = os.WriteFile("frege_refresh_token", []byte(refresh), 0o600) // it rotates
	}),
)
client := frege.New(tok)
```

A user token also reaches endpoints an API key cannot, such as
`client.ListProjects(ctx, orgID)`.

## When you want an agent, not a script

This SDK is the deterministic path: your code decides which tool to run. If you
instead want an AI agent to pick and chain tools, Frege's project is already a
standard **MCP server**. Point the official
[`modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk)
client at your project's clean URL and it just works:

```
https://frege.io/mcp/{org-slug}/{project-slug}
```

That path uses a different token (an MCP-audience OAuth token, not the one this
SDK uses). See the Frege docs for the MCP connect flow.

## Reference

| Method | What it does | API key? |
|---|---|---|
| `client.InvokeTool(ctx, projectID, name, args, opts…)` | Run one tool | ✅ |
| `client.ListOperations(ctx, projectID)` | The project's tools + input schemas | ✅ |
| `client.ListProjects(ctx, orgID)` | Projects in one organization | user only |
| `frege.SendMagicCode(ctx, baseURL, email)` | Email a sign-in code | — |
| `frege.VerifyMagicCode(ctx, baseURL, email, code)` | Exchange the code for a `Session` | — |
| `frege.NewRefreshingToken(access, refresh, opts…)` | A user-token source that auto-refreshes | — |

For a self-serve project (each customer has their own credential), pass
`frege.AsClient(customerID)` to `InvokeTool`. All calls take a
`context.Context`, so timeouts and cancellation work as usual.
