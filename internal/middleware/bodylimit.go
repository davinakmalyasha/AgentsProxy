// DONE: perf (bounded JSON bodies) / STANDARDS A, D
package middleware

import "net/http"

// MaxProjectJSONBody caps project/vertical JSON writes. Valid payloads are
// KBs (booking create, facility patch); 1 MiB is generous and prevents a
// single huge body from OOMing a free-plan instance. Larger surfaces
// (report .xlsx downloads are GETs, agency uploads live outside this
// router) are unaffected. Handlers keep their own json.Decode errors as
// 400; oversize bodies surface as 400 on the next Decode, not a hang.
const MaxProjectJSONBody = 1 << 20 // 1 MiB

// LimitJSONBody bounds POST/PATCH/PUT bodies. GET/DELETE/HEAD pass
// through untouched. Wrapping twice (e.g. chat's own MaxBytesReader)
// is harmless: the inner limit still enforces on Decode.
func LimitJSONBody(maxBytes int64) func(http.Handler) http.Handler {
	if maxBytes <= 0 {
		maxBytes = MaxProjectJSONBody
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodPost, http.MethodPatch, http.MethodPut:
				r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
			}
			next.ServeHTTP(w, r)
		})
	}
}
