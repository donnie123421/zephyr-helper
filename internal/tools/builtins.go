package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/donnie123421/zephyr-helper/internal/truenas"
)

// builtins returns the tool set this helper registers when TrueNAS is
// configured. All v1 tools are read-only — nothing here mutates state.
func builtins(tn *truenas.Client) []Tool {
	return []Tool{
		listPools(tn),
		poolStatus(tn),
		listApps(tn),
		listDisks(tn),
		listAlerts(tn),
		systemInfo(tn),
	}
}

// JSON Schema for the common "no arguments" shape. Declared once so every
// no-arg tool references the same bytes.
var noArgs = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

// ----- list_pools ------------------------------------------------------------

func listPools(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "list_pools",
			Description: "List all ZFS pools on the TrueNAS server with their name, " +
				"health, status, total size, and used space. Use this when the user asks " +
				"about pools, storage usage, or pool health.",
			Parameters: noArgs,
		},
		StatusLine: "Checking pools…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			raw, err := tn.GetRaw(ctx, "/pool")
			if err != nil {
				return "", err
			}
			var pools []map[string]any
			if err := json.Unmarshal(raw, &pools); err != nil {
				return "", fmt.Errorf("decode pools: %w", err)
			}

			out := make([]map[string]any, 0, len(pools))
			for _, p := range pools {
				entry := map[string]any{}
				copyFields(entry, p, "name", "status", "healthy", "size", "allocated", "free")
				if scan, ok := p["scan"].(map[string]any); ok {
					entry["scan"] = projectScan(scan)
				}
				out = append(out, entry)
			}
			return toJSON(map[string]any{"pools": out})
		},
	}
}

// ----- pool_status -----------------------------------------------------------

func poolStatus(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "pool_status",
			Description: "Get detailed status for a single ZFS pool, including health, " +
				"vdev layout summary, scan progress (scrub/resilver), and error counts. " +
				"Call list_pools first if you don't know the pool name.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pool_name": {
						"type": "string",
						"description": "The pool name, e.g. \"tank\"."
					}
				},
				"required": ["pool_name"],
				"additionalProperties": false
			}`),
		},
		StatusLine: "Checking pool status…",
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				PoolName string `json:"pool_name"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("bad args: %w", err)
			}
			if a.PoolName == "" {
				return "", fmt.Errorf("pool_name is required")
			}

			// TrueNAS /pool/id/{name} accepts the pool name directly.
			raw, err := tn.GetRaw(ctx, "/pool/id/"+url.PathEscape(a.PoolName))
			if err != nil {
				return "", err
			}
			var p map[string]any
			if err := json.Unmarshal(raw, &p); err != nil {
				return "", fmt.Errorf("decode pool: %w", err)
			}

			out := map[string]any{}
			copyFields(out, p, "name", "status", "healthy", "size", "allocated", "free")
			if scan, ok := p["scan"].(map[string]any); ok {
				out["scan"] = projectScan(scan)
			}
			if topo, ok := p["topology"].(map[string]any); ok {
				out["topology_summary"] = summarizeTopology(topo)
			}
			return toJSON(out)
		},
	}
}

// ----- list_apps -------------------------------------------------------------

func listApps(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "list_apps",
			Description: "List installed TrueNAS applications (Docker-based charts) " +
				"with their name, running state, version, and source catalog.",
			Parameters: noArgs,
		},
		StatusLine: "Checking apps…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			raw, err := tn.GetRaw(ctx, "/app")
			if err != nil {
				return "", err
			}
			var apps []map[string]any
			if err := json.Unmarshal(raw, &apps); err != nil {
				return "", fmt.Errorf("decode apps: %w", err)
			}

			out := make([]map[string]any, 0, len(apps))
			for _, a := range apps {
				entry := map[string]any{}
				copyFields(entry, a, "name", "state", "version", "upgrade_available", "catalog", "train")
				if m, ok := a["metadata"].(map[string]any); ok {
					copyFields(entry, m, "title", "app_version")
				}
				out = append(out, entry)
			}
			return toJSON(map[string]any{"apps": out})
		},
	}
}

// ----- list_disks ------------------------------------------------------------

func listDisks(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "list_disks",
			Description: "List physical disks on the NAS with model, serial, size, " +
				"type (HDD/SSD), and which pool (if any) they're assigned to.",
			Parameters: noArgs,
		},
		StatusLine: "Checking disks…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			raw, err := tn.GetRaw(ctx, "/disk")
			if err != nil {
				return "", err
			}
			var disks []map[string]any
			if err := json.Unmarshal(raw, &disks); err != nil {
				return "", fmt.Errorf("decode disks: %w", err)
			}

			out := make([]map[string]any, 0, len(disks))
			for _, d := range disks {
				entry := map[string]any{}
				copyFields(entry, d,
					"name", "devname", "identifier",
					"model", "serial", "size", "type", "pool")
				out = append(out, entry)
			}
			return toJSON(map[string]any{"disks": out})
		},
	}
}

// ----- list_alerts -----------------------------------------------------------

func listAlerts(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "list_alerts",
			Description: "List currently active (non-dismissed) TrueNAS alerts with " +
				"severity, timestamp, and message. Use this when the user asks about " +
				"alerts, warnings, errors, or 'what's wrong with my NAS'.",
			Parameters: noArgs,
		},
		StatusLine: "Checking alerts…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			raw, err := tn.GetRaw(ctx, "/alert/list")
			if err != nil {
				return "", err
			}
			var alerts []map[string]any
			if err := json.Unmarshal(raw, &alerts); err != nil {
				return "", fmt.Errorf("decode alerts: %w", err)
			}

			out := make([]map[string]any, 0, len(alerts))
			for _, a := range alerts {
				// Skip dismissed alerts — the LLM doesn't need to reason about them.
				if dismissed, ok := a["dismissed"].(bool); ok && dismissed {
					continue
				}
				entry := map[string]any{}
				copyFields(entry, a,
					"uuid", "level", "formatted", "text",
					"datetime", "last_occurrence", "klass", "node")
				out = append(out, entry)
			}
			return toJSON(map[string]any{"alerts": out})
		},
	}
}

// ----- system_info -----------------------------------------------------------

func systemInfo(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "system_info",
			Description: "Get TrueNAS system info: version, hostname, uptime, model, " +
				"RAM, and CPU. Use when the user asks about the server itself " +
				"(not pools or apps).",
			Parameters: noArgs,
		},
		StatusLine: "Checking system info…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			raw, err := tn.GetRaw(ctx, "/system/info")
			if err != nil {
				return "", err
			}
			var info map[string]any
			if err := json.Unmarshal(raw, &info); err != nil {
				return "", fmt.Errorf("decode info: %w", err)
			}

			out := map[string]any{}
			copyFields(out, info,
				"version", "hostname", "uptime", "uptime_seconds",
				"system_product", "system_manufacturer", "physmem",
				"model", "cores", "physical_cores",
				"loadavg", "timezone", "system_serial")
			return toJSON(out)
		},
	}
}

// ----- helpers ---------------------------------------------------------------

// copyFields shallow-copies whitelisted keys from src into dst. Keys absent
// from src are silently skipped so tool handlers stay resilient to TrueNAS
// schema drift.
func copyFields(dst, src map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := src[k]; ok {
			dst[k] = v
		}
	}
}

// projectScan trims the huge raw scan object down to the fields the LLM cares
// about — function (scrub/resilver/none), state, progress, errors, timestamps.
func projectScan(scan map[string]any) map[string]any {
	out := map[string]any{}
	copyFields(out, scan,
		"function", "state", "percentage", "errors",
		"start_time", "end_time",
		"total_secs_left", "bytes_processed", "bytes_to_process",
	)
	return out
}

// summarizeTopology counts vdevs per section (data/cache/log/spare) instead of
// passing the full nested disk layout through — raw topology objects can run
// to kilobytes per pool.
func summarizeTopology(topo map[string]any) map[string]any {
	summary := map[string]any{}
	for section, v := range topo {
		vdevs, ok := v.([]any)
		if !ok {
			continue
		}
		section_summary := make([]map[string]any, 0, len(vdevs))
		for _, vd := range vdevs {
			m, ok := vd.(map[string]any)
			if !ok {
				continue
			}
			entry := map[string]any{}
			copyFields(entry, m, "name", "type", "status")
			if children, ok := m["children"].([]any); ok {
				entry["child_count"] = len(children)
			}
			section_summary = append(section_summary, entry)
		}
		summary[section] = section_summary
	}
	return summary
}

func toJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal: %w", err)
	}
	return string(b), nil
}
