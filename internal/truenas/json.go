package truenas

import "encoding/json"

// jsonUnmarshalImpl exists so mapper.go can swap out the implementation
// in tests while keeping production code free of the encoding/json
// import. Production: standard library; tests: anything they want.
func jsonUnmarshalImpl(s string, v any) error {
	return json.Unmarshal([]byte(s), v)
}
