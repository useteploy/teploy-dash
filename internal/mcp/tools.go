package mcp

import (
	"context"
	"fmt"
)

// Tool is one MCP tool. InputSchema is a JSON-Schema object; Run receives the
// already-decoded arguments and returns human/agent-readable text.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]interface{}
	ReadOnly    bool
	Destructive bool
	Run         func(ctx context.Context, args map[string]interface{}) (string, error)
}

// Backend is what the tools need from the dashboard. Implemented by
// server.Server over its EXISTING read paths (state files / SSH readers) and
// the EXISTING teploy-CLI delegate — tools must never grow their own state
// or bypass the CLI for mutations; that invariant is what keeps MCP free of
// sync drift.
type Backend interface {
	ListApps(ctx context.Context) (string, error)
	GetApp(ctx context.Context, server, app string) (string, error)
	AppLogs(ctx context.Context, server, app string, lines int) (string, error)
	ListServers(ctx context.Context) (string, error)
	ListMonitors(ctx context.Context) (string, error)
	ListEnvKeys(ctx context.Context, server, app string) (string, error)

	Deploy(ctx context.Context, server, app, image, domain string, port int) (string, error)
	Rollback(ctx context.Context, server, app string) (string, error)
	AppAction(ctx context.Context, server, app, action string) (string, error)
	SetEnv(ctx context.Context, server, app, key, value string) (string, error)
	UnsetEnv(ctx context.Context, server, app, key string) (string, error)
}

func schema(required []string, props map[string]interface{}) map[string]interface{} {
	if props == nil {
		props = map[string]interface{}{}
	}
	s := map[string]interface{}{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func strProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func intProp(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc}
}

func strArg(args map[string]interface{}, key string) (string, error) {
	v, ok := args[key].(string)
	if !ok || v == "" {
		return "", fmt.Errorf("missing required argument: %s", key)
	}
	return v, nil
}

func serverApp(args map[string]interface{}) (string, string, error) {
	server, err := strArg(args, "server")
	if err != nil {
		return "", "", err
	}
	app, err := strArg(args, "app")
	if err != nil {
		return "", "", err
	}
	return server, app, nil
}

var serverAppSchema = schema([]string{"server", "app"}, map[string]interface{}{
	"server": strProp("Server name (as listed by teploy_list_servers)"),
	"app":    strProp("App name"),
})

// simpleAction builds a mutating tool that delegates one app action verb to
// the CLI (the same call the dashboard's button makes).
func simpleAction(name, description, verb string, destructive bool, b Backend) Tool {
	return Tool{
		Name:        name,
		Description: description,
		InputSchema: serverAppSchema,
		Destructive: destructive,
		Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
			server, app, err := serverApp(args)
			if err != nil {
				return "", err
			}
			return b.AppAction(ctx, server, app, verb)
		},
	}
}

// Tools builds the curated v1 tool set over a backend.
func Tools(b Backend) []Tool {
	return []Tool{
		// ── Reads ────────────────────────────────────────────────────────
		{
			Name:        "teploy_list_apps",
			Description: "List every deployed app across all servers with status, version, and domain. This is read live from the state files the teploy CLI writes on each server — it cannot be stale relative to reality.",
			InputSchema: schema(nil, nil),
			ReadOnly:    true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				return b.ListApps(ctx)
			},
		},
		{
			Name:        "teploy_get_app",
			Description: "Get the deployment state of one app on one server (status, current and previous version, domain).",
			InputSchema: serverAppSchema,
			ReadOnly:    true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				server, app, err := serverApp(args)
				if err != nil {
					return "", err
				}
				return b.GetApp(ctx, server, app)
			},
		},
		{
			Name:        "teploy_app_logs",
			Description: "Fetch recent container logs for an app (bounded tail, not a stream).",
			InputSchema: schema([]string{"server", "app"}, map[string]interface{}{
				"server": strProp("Server name"),
				"app":    strProp("App name"),
				"lines":  intProp("Number of log lines (default 100, max 500)"),
			}),
			ReadOnly: true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				server, app, err := serverApp(args)
				if err != nil {
					return "", err
				}
				lines := 100
				if n, ok := args["lines"].(float64); ok && n > 0 {
					lines = int(n)
				}
				if lines > 500 {
					lines = 500
				}
				return b.AppLogs(ctx, server, app, lines)
			},
		},
		{
			Name:        "teploy_list_servers",
			Description: "List the servers teploy knows about (names and hosts).",
			InputSchema: schema(nil, nil),
			ReadOnly:    true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				return b.ListServers(ctx)
			},
		},
		{
			Name:        "teploy_list_monitors",
			Description: "List uptime monitors with their current up/down state and 24h uptime.",
			InputSchema: schema(nil, nil),
			ReadOnly:    true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				return b.ListMonitors(ctx)
			},
		},
		{
			Name:        "teploy_list_env_keys",
			Description: "List the environment variable NAMES configured for an app. Values are never returned over MCP.",
			InputSchema: serverAppSchema,
			ReadOnly:    true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				server, app, err := serverApp(args)
				if err != nil {
					return "", err
				}
				return b.ListEnvKeys(ctx, server, app)
			},
		},

		// ── Actions (all delegate to the teploy CLI — same path as the UI) ──
		{
			Name:        "teploy_deploy",
			Description: "Deploy an image as an app on a server (zero-downtime; health-checked with automatic rollback). Same ad-hoc deploy the dashboard performs.",
			InputSchema: schema([]string{"server", "app", "image"}, map[string]interface{}{
				"server": strProp("Server name"),
				"app":    strProp("App name"),
				"image":  strProp("Docker image reference to deploy"),
				"domain": strProp("Domain to route (optional for redeploys)"),
				"port":   intProp("Container port (default 80)"),
			}),
			Destructive: true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				server, app, err := serverApp(args)
				if err != nil {
					return "", err
				}
				image, err := strArg(args, "image")
				if err != nil {
					return "", err
				}
				domain, _ := args["domain"].(string)
				port := 0
				if n, ok := args["port"].(float64); ok {
					port = int(n)
				}
				return b.Deploy(ctx, server, app, image, domain, port)
			},
		},
		{
			Name:        "teploy_rollback",
			Description: "Roll an app back to its previous version.",
			InputSchema: serverAppSchema,
			Destructive: true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				server, app, err := serverApp(args)
				if err != nil {
					return "", err
				}
				return b.Rollback(ctx, server, app)
			},
		},
		simpleAction("teploy_restart", "Restart an app's containers.", "restart", true, b),
		simpleAction("teploy_stop", "Stop an app's containers.", "stop", true, b),
		simpleAction("teploy_start", "Start an app's stopped containers.", "start", false, b),
		simpleAction("teploy_lock", "Acquire the deploy lock for an app (blocks other deploys).", "lock", false, b),
		simpleAction("teploy_unlock", "Release an app's deploy lock.", "unlock", false, b),
		simpleAction("teploy_maintenance_on", "Enable maintenance mode (serves a maintenance page).", "maintenance on", false, b),
		simpleAction("teploy_maintenance_off", "Disable maintenance mode.", "maintenance off", false, b),
		{
			Name:        "teploy_set_env",
			Description: "Set an environment variable for an app (applies on next deploy/restart).",
			InputSchema: schema([]string{"server", "app", "key", "value"}, map[string]interface{}{
				"server": strProp("Server name"),
				"app":    strProp("App name"),
				"key":    strProp("Variable name"),
				"value":  strProp("Variable value"),
			}),
			Destructive: true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				server, app, err := serverApp(args)
				if err != nil {
					return "", err
				}
				key, err := strArg(args, "key")
				if err != nil {
					return "", err
				}
				value, ok := args["value"].(string)
				if !ok {
					return "", fmt.Errorf("missing required argument: value")
				}
				return b.SetEnv(ctx, server, app, key, value)
			},
		},
		{
			Name:        "teploy_unset_env",
			Description: "Remove an environment variable from an app.",
			InputSchema: schema([]string{"server", "app", "key"}, map[string]interface{}{
				"server": strProp("Server name"),
				"app":    strProp("App name"),
				"key":    strProp("Variable name"),
			}),
			Destructive: true,
			Run: func(ctx context.Context, args map[string]interface{}) (string, error) {
				server, app, err := serverApp(args)
				if err != nil {
					return "", err
				}
				key, err := strArg(args, "key")
				if err != nil {
					return "", err
				}
				return b.UnsetEnv(ctx, server, app, key)
			},
		},
	}
}
