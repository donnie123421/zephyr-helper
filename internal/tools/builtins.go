package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

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
		listShares(tn),
		listDatasets(tn),
		listSnapshots(tn),
		listSmartTests(tn),
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
			Description: "Get detailed status for ZFS pools (health, vdev layout, " +
				"scan progress, error counts). Omit pool_name to get detail for " +
				"every pool on the server — prefer this when you don't already " +
				"know the exact pool name from list_pools.",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pool_name": {
						"type": "string",
						"description": "Exact pool name as returned by list_pools. Omit this field entirely to get status for all pools."
					}
				},
				"additionalProperties": false
			}`),
		},
		StatusLine: "Checking pool status…",
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				PoolName string `json:"pool_name"`
			}
			// Args may be empty bytes or the literal "null" when the model
			// calls the tool with no arguments — both are valid now.
			if len(args) > 0 && string(args) != "null" {
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("bad args: %w", err)
				}
			}

			if a.PoolName == "" {
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
					out = append(out, projectPoolDetail(p))
				}
				return toJSON(map[string]any{"pools": out})
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
			return toJSON(projectPoolDetail(p))
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

// ----- list_shares -----------------------------------------------------------

func listShares(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "list_shares",
			Description: "List all file-sharing exports on the NAS: SMB (Windows/Mac), " +
				"NFS (Unix), and iSCSI targets. Each entry includes type, name, path, " +
				"and enabled state. Use when the user asks 'what am I sharing', 'what " +
				"SMB/NFS shares do I have', or about network shares generally.",
			Parameters: noArgs,
		},
		StatusLine: "Checking shares…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			out := make([]map[string]any, 0)

			// SMB: most home users live here. Surface name + path + enabled
			// + purpose (e.g. DEFAULT_SHARE vs TIME_MACHINE) so the LLM can
			// answer "is this Time Machine?" without a follow-up call.
			if raw, err := tn.GetRaw(ctx, "/sharing/smb"); err == nil {
				var smb []map[string]any
				if json.Unmarshal(raw, &smb) == nil {
					for _, s := range smb {
						entry := map[string]any{"type": "smb"}
						copyFields(entry, s, "name", "path", "enabled", "comment", "purpose", "locked")
						out = append(out, entry)
					}
				}
			}

			// NFS doesn't have a "name" — path is the identifier. Hosts /
			// networks are the access control so include them.
			if raw, err := tn.GetRaw(ctx, "/sharing/nfs"); err == nil {
				var nfs []map[string]any
				if json.Unmarshal(raw, &nfs) == nil {
					for _, s := range nfs {
						entry := map[string]any{"type": "nfs"}
						copyFields(entry, s, "path", "enabled", "comment", "hosts", "networks", "locked")
						out = append(out, entry)
					}
				}
			}

			// iSCSI targets are block-level exports — users treat them like
			// a remote disk. Name = IQN, alias is the friendly label.
			if raw, err := tn.GetRaw(ctx, "/iscsi/target"); err == nil {
				var targets []map[string]any
				if json.Unmarshal(raw, &targets) == nil {
					for _, t := range targets {
						entry := map[string]any{"type": "iscsi"}
						copyFields(entry, t, "name", "alias", "mode")
						out = append(out, entry)
					}
				}
			}

			return toJSON(map[string]any{"shares": out})
		},
	}
}

// ----- list_datasets ---------------------------------------------------------

func listDatasets(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "list_datasets",
			Description: "List ZFS datasets (filesystems and zvols) with name, type, " +
				"used/available bytes, mountpoint, quota, and encryption state. Use " +
				"when the user asks about dataset layout, space per dataset, or " +
				"which datasets are encrypted.",
			Parameters: noArgs,
		},
		StatusLine: "Checking datasets…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			raw, err := tn.GetRaw(ctx, "/pool/dataset")
			if err != nil {
				return "", err
			}
			var datasets []map[string]any
			if err := json.Unmarshal(raw, &datasets); err != nil {
				return "", fmt.Errorf("decode datasets: %w", err)
			}

			out := make([]map[string]any, 0, len(datasets))
			for _, d := range datasets {
				entry := map[string]any{}
				copyFields(entry, d, "name", "type", "pool", "mountpoint", "encrypted", "locked")
				entry["used_bytes"] = zfsPropBytes(d["used"])
				entry["available_bytes"] = zfsPropBytes(d["available"])
				entry["quota_bytes"] = zfsPropBytes(d["quota"])
				out = append(out, entry)
			}
			return toJSON(map[string]any{"datasets": out})
		},
	}
}

// ----- list_snapshots --------------------------------------------------------

func listSnapshots(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "list_snapshots",
			Description: "List ZFS snapshots, newest first, capped at 50. Pass " +
				"dataset to filter to a single dataset (use this when the user " +
				"names one — the raw snapshot list can run to thousands of rows " +
				"and the LLM doesn't need that much context).",
			Parameters: json.RawMessage(`{
				"type": "object",
				"properties": {
					"dataset": {
						"type": "string",
						"description": "Exact dataset name to filter by, e.g. 'tank/media'. Omit to list recent snapshots across all datasets."
					}
				},
				"additionalProperties": false
			}`),
		},
		StatusLine: "Checking snapshots…",
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Dataset string `json:"dataset"`
			}
			if len(args) > 0 && string(args) != "null" {
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("bad args: %w", err)
				}
			}

			// TrueNAS /zfs/snapshot supports queryset filters + ordering +
			// limit. Newest-first ordering matters because without it the
			// middleware returns the oldest 50, which is the opposite of
			// what a user wants.
			params := url.Values{}
			params.Set("limit", "50")
			params.Set("order_by", "-created")
			if a.Dataset != "" {
				params.Set("filters", fmt.Sprintf(`[["dataset","=","%s"]]`, a.Dataset))
			}
			path := "/zfs/snapshot?" + params.Encode()

			raw, err := tn.GetRaw(ctx, path)
			if err != nil {
				return "", err
			}
			var snaps []map[string]any
			if err := json.Unmarshal(raw, &snaps); err != nil {
				return "", fmt.Errorf("decode snapshots: %w", err)
			}

			out := make([]map[string]any, 0, len(snaps))
			for _, s := range snaps {
				entry := map[string]any{}
				copyFields(entry, s, "name", "dataset", "snapshot_name", "created")
				if props, ok := s["properties"].(map[string]any); ok {
					entry["used_bytes"] = zfsPropBytes(props["used"])
					entry["referenced_bytes"] = zfsPropBytes(props["referenced"])
				}
				out = append(out, entry)
			}
			result := map[string]any{
				"snapshots": out,
				"count":     len(out),
			}
			if a.Dataset != "" {
				result["dataset_filter"] = a.Dataset
			}
			return toJSON(result)
		},
	}
}

// ----- list_smart_tests ------------------------------------------------------

func listSmartTests(tn *truenas.Client) Tool {
	return Tool{
		Def: Definition{
			Name: "list_smart_tests",
			Description: "Get SMART self-test results for each physical disk. " +
				"Each entry lists the disk and its most-recent test type, status, " +
				"and timestamp. Use when the user asks about disk health, SMART " +
				"errors, or 'are my drives failing'.",
			Parameters: noArgs,
		},
		StatusLine: "Checking SMART tests…",
		Handler: func(ctx context.Context, _ json.RawMessage) (string, error) {
			// /smart/test/results returns [{disk, tests: [...]}] where tests
			// are newest-first. We project the most recent per disk so the
			// LLM gets a compact per-disk health view instead of an N×M
			// history matrix.
			raw, err := tn.GetRaw(ctx, "/smart/test/results")
			if err != nil {
				return "", err
			}
			var results []map[string]any
			if err := json.Unmarshal(raw, &results); err != nil {
				return "", fmt.Errorf("decode smart results: %w", err)
			}

			out := make([]map[string]any, 0, len(results))
			for _, r := range results {
				entry := map[string]any{}
				copyFields(entry, r, "disk")
				tests, _ := r["tests"].([]any)
				if len(tests) > 0 {
					if latest, ok := tests[0].(map[string]any); ok {
						copyFields(entry, latest,
							"num", "description", "status", "status_verbose",
							"remaining", "lifetime", "lba_of_first_error")
					}
				}
				entry["test_count"] = len(tests)
				out = append(out, entry)
			}
			return toJSON(map[string]any{"disks": out})
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

// projectPoolDetail projects a single /pool entry down to the fields the LLM
// can usefully reason about. Shared between pool_status (single pool) and
// pool_status (all pools) so their output shapes stay identical.
func projectPoolDetail(p map[string]any) map[string]any {
	out := map[string]any{}
	copyFields(out, p, "name", "status", "healthy", "size", "allocated", "free")
	if scan, ok := p["scan"].(map[string]any); ok {
		out["scan"] = projectScan(scan)
	}
	if topo, ok := p["topology"].(map[string]any); ok {
		out["topology_summary"] = summarizeTopology(topo)
	}
	return out
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

// zfsPropBytes extracts a byte count from a ZFS property field. Modern
// TrueNAS wraps numeric props in {parsed, rawvalue, value, source}, but
// older versions / absent fields can come through as a bare number,
// a string, or nil. Returns 0 when we can't derive a number — a missing
// value is better than a wrong one.
func zfsPropBytes(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int64:
		return x
	case map[string]any:
		if p, ok := x["parsed"].(float64); ok {
			return int64(p)
		}
		if rv, ok := x["rawvalue"].(string); ok {
			if n, err := strconv.ParseInt(rv, 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}
