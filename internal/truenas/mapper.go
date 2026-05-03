// REST → JSON-RPC translation table for the helper's truenas client.
// Mirrors the iOS EndpointMapper but only covers the paths the helper
// actually calls (16 endpoints across pollers, tools, and remote-
// access). Adding a new path the helper needs means adding an entry
// here, not changing every call site.
package truenas

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// methodKind controls how the path's optional /id/{value} segment
// gets folded into the JSON-RPC params.
type methodKind int

const (
	// kindQuery: GET → method.query with [filters, options].
	// /resource → method.query [[]]
	// /resource/id/X → method.query with filter [["id","=",X]] and {get:true}
	kindQuery methodKind = iota
	// kindBare: method called with no inferred filter shape. Used for
	// alert.list, system.info, smart.test.results — these accept
	// either no params or query-options-as-first-arg, but never a
	// filter-as-first-arg the way .query methods do.
	kindBare
	// kindConfig: GET → method.config (singleton). Used for
	// network.configuration, system.general, etc.
	kindConfig
	// kindAction: bare method invoked with body as first param.
	// Used for /audit/query and similar POST-style actions.
	kindAction
)

type override struct {
	rpcMethod string
	kind      methodKind
}

// overrides maps normalised paths (no /id/X, no query string) to a
// concrete JSON-RPC method when the generic translation rules don't
// fit. Same rules the iOS EndpointMapper uses, narrowed to the
// helper's actual REST surface.
var overrides = map[string]override{
	// Bare GETs that are NOT .query
	"/system/info":         {"system.info", kindBare},
	"/alert/list":          {"alert.list", kindBare},
	"/smart/test/results":  {"smart.test.results", kindBare},
	"/core/get_jobs":       {"core.get_jobs", kindBare},

	// Config singletons
	"/system/general":         {"system.general", kindConfig},
	"/network/configuration":  {"network.configuration", kindConfig},

	// Action POSTs
	"/audit/query": {"audit.query", kindAction},
}

// mapped is the resolved RPC call shape. Caller invokes
// `client.Call(ctx, m.method, m.params)`.
type mapped struct {
	method string
	params []any
}

// mapREST translates `httpMethod path` (+ optional body for POST)
// into the JSON-RPC method + params the helper's RPC client expects.
//
// Paths supported:
//   - /resource                             → resource.query
//   - /resource/id/X                        → resource.query, {get:true} on result
//   - /resource?key=val&key=val             → query options become filters
//   - overrides table for the special cases
func mapREST(httpMethod, path string, body any) (mapped, error) {
	pathOnly, query := splitQuery(path)
	normalisedPath, idValue := extractID(pathOnly)

	// 1. Override hit — caller has hand-tuned the translation.
	if ov, ok := overrides[normalisedPath]; ok {
		return resolveOverride(ov, idValue, query, httpMethod, body)
	}

	// 2. Generic translation for anything not in the override table.
	resource := resourceFromPath(normalisedPath)
	if resource == "" {
		return mapped{}, fmt.Errorf("rest map: empty resource in %q", path)
	}

	switch strings.ToUpper(httpMethod) {
	case "GET":
		return mappedQuery(resource+".query", idValue, query), nil
	case "POST":
		return mapped{method: resource + ".create", params: actionParams(nil, body)}, nil
	default:
		return mapped{}, fmt.Errorf("rest map: unsupported method %q for %q", httpMethod, path)
	}
}

func resolveOverride(
	ov override,
	idValue any,
	query string,
	httpMethod string,
	body any,
) (mapped, error) {
	switch ov.kind {
	case kindBare:
		params := bareParams(query)
		return mapped{method: ov.rpcMethod, params: params}, nil
	case kindConfig:
		// GET → .config (no params); PUT/POST → .update (with body)
		switch strings.ToUpper(httpMethod) {
		case "GET":
			return mapped{method: ov.rpcMethod + ".config"}, nil
		default:
			return mapped{method: ov.rpcMethod + ".update", params: actionParams(nil, body)}, nil
		}
	case kindAction:
		// audit.query has a quirky shape: REST clients POST a body
		// dict with `query-filters` + `query-options` keys, but the
		// JSON-RPC method takes them as two positional args. Unpack
		// here so the helper's pollers don't need to know about the
		// transport difference.
		if ov.rpcMethod == "audit.query" {
			return mapped{method: ov.rpcMethod, params: auditQueryParams(body)}, nil
		}
		return mapped{method: ov.rpcMethod, params: actionParams(idValue, body)}, nil
	case kindQuery:
		return mappedQuery(ov.rpcMethod, idValue, query), nil
	}
	return mapped{}, errors.New("rest map: unknown override kind")
}

// mappedQuery builds the params for a `.query` call. Single-record
// fetches (path has /id/X) add `{get: true}` to the options dict so
// TrueNAS returns a single object instead of an array — matching the
// REST GET /resource/id/X behaviour we're replacing.
func mappedQuery(method string, idValue any, query string) mapped {
	filters := parseFilters(query)
	options := parseOptions(query)

	if idValue != nil {
		filters = append(filters, []any{"id", "=", idValue})
		options["get"] = true
	}

	if len(filters) == 0 && len(options) == 0 {
		return mapped{method: method, params: []any{}}
	}
	if len(options) == 0 {
		return mapped{method: method, params: []any{filters}}
	}
	return mapped{method: method, params: []any{filters, options}}
}

// bareParams builds the params for a bare RPC method that accepts
// optional query-options. TrueNAS query-style methods (`core.get_jobs`,
// `alert.list`, etc.) take exactly two positional args when any are
// supplied — `(filters, options)` — even when one side is empty.
// `core.get_jobs?limit=100` rejected `[options]` with -32602 because
// it expected `[[], options]`. We always emit both when *either*
// side is non-empty so TrueNAS sees the shape it expects.
//
// Returns nil when the call has no query string at all — most bare
// methods (system.info, alert.list with no filter, smart.test.results)
// take no arguments and choke on a spurious empty filter list.
func bareParams(query string) []any {
	options := parseOptions(query)
	filters := parseFilters(query)
	if len(filters) == 0 && len(options) == 0 {
		return nil
	}
	if filters == nil {
		filters = []any{}
	}
	if options == nil {
		options = map[string]any{}
	}
	return []any{filters, options}
}

// auditQueryParams forwards the body to TrueNAS's audit.query as a
// single positional dict — that's the shape the JSON-RPC method
// expects (verified empirically: positional [filters, options]
// returns -32602 Invalid params, while [body] works).
//
// The helper's pollers/security.go already builds the right dict
// shape: {query-filters: [...], query-options: {...}}. We just
// wrap it in a one-element params array.
func auditQueryParams(body any) []any {
	if body == nil {
		return []any{map[string]any{
			"query-filters": []any{},
			"query-options": map[string]any{},
		}}
	}
	return []any{body}
}

// actionParams packs (id?, body?) into positional JSON-RPC params.
// Omits nil entries so TrueNAS doesn't see a literal null where it
// expects "no argument."
func actionParams(idValue any, body any) []any {
	var out []any
	if idValue != nil {
		out = append(out, idValue)
	}
	if body != nil {
		out = append(out, body)
	}
	return out
}

// splitQuery returns (pathOnly, query) — the query string never
// reaches the RPC method name.
func splitQuery(path string) (string, string) {
	if i := strings.IndexByte(path, '?'); i >= 0 {
		return path[:i], path[i+1:]
	}
	return path, ""
}

// extractID strips a `/id/{value}` segment from the path and returns
// the path with that segment removed plus the id (Int when numeric,
// otherwise the raw string). Handles URL-escaped values so app names
// with special chars survive the round-trip.
func extractID(path string) (string, any) {
	parts := strings.FieldsFunc(path, func(r rune) bool { return r == '/' })
	for i, p := range parts {
		if p == "id" && i+1 < len(parts) {
			rawID, err := url.PathUnescape(parts[i+1])
			if err != nil {
				rawID = parts[i+1]
			}
			head := parts[:i]
			tail := parts[i+2:]
			rebuilt := "/" + strings.Join(head, "/")
			if len(tail) > 0 {
				rebuilt += "/" + strings.Join(tail, "/")
			}
			return rebuilt, idValueFromString(rawID)
		}
	}
	return path, nil
}

// idValueFromString returns an int when the id parses cleanly, else
// the raw string. TrueNAS distinguishes — `pool.query` with id=123
// vs id="tank" — so don't auto-convert string-shaped ids.
func idValueFromString(s string) any {
	// Try int parse. Don't use strconv.Atoi — too permissive on
	// leading + signs etc.
	for _, r := range s {
		if r < '0' || r > '9' {
			return s
		}
	}
	if s == "" {
		return s
	}
	var n int64
	for _, r := range s {
		n = n*10 + int64(r-'0')
	}
	return n
}

// resourceFromPath turns "/pool/dataset" into "pool.dataset".
func resourceFromPath(path string) string {
	return strings.Join(strings.FieldsFunc(path, func(r rune) bool { return r == '/' }), ".")
}

// parseFilters extracts a TrueNAS filter list from the query string.
// `filters=[["dataset","=","tank"]]` arrives URL-encoded; we let the
// caller pre-encode and we just JSON-parse it back.
func parseFilters(query string) []any {
	values, err := url.ParseQuery(query)
	if err != nil {
		return nil
	}
	raw := values.Get("filters")
	if raw == "" {
		return nil
	}
	var parsed []any
	if err := jsonUnmarshal(raw, &parsed); err != nil {
		return nil
	}
	return parsed
}

// parseOptions builds a query-options map from URL params. Honours
// limit, offset, order_by, and the bare get/count flags TrueNAS
// recognises.
func parseOptions(query string) map[string]any {
	values, err := url.ParseQuery(query)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if v := values.Get("limit"); v != "" {
		if n, err := atoi(v); err == nil {
			out["limit"] = n
		}
	}
	if v := values.Get("offset"); v != "" {
		if n, err := atoi(v); err == nil {
			out["offset"] = n
		}
	}
	if v := values.Get("order_by"); v != "" {
		out["order_by"] = []any{v}
	}
	if v := values.Get("get"); v == "true" {
		out["get"] = true
	}
	return out
}

// jsonUnmarshal is wrapped so the unit tests can swap it out, and so
// the mapper file doesn't have to import encoding/json directly
// (keeps the surface obvious from the function signature alone).
var jsonUnmarshal = func(s string, v any) error {
	return jsonUnmarshalImpl(s, v)
}

// atoi without strconv to keep the lints unhappy about NumberFormat
// edge cases. Pure decimal, no leading +, no whitespace.
func atoi(s string) (int, error) {
	if s == "" {
		return 0, errors.New("atoi: empty")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("atoi: non-digit %q", r)
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}
