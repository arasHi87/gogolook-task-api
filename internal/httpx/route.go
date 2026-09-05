package httpx

import "strings"

// Route templates a request path into one of a fixed set of route labels.
//
// This exists for cardinality. The route is a metric label and a log field, and
// a label built from the raw URL is unbounded and attacker-controlled: one
// scanner walking /tasks/<random-uuid> would mint a new time series per request
// and take the metrics backend down. Returning RouteOther for anything
// unrecognised makes the label set finite by construction, and the set is small
// enough to write down — which is the test.
const (
	// RouteOther is every path that is not one of ours.
	RouteOther = "other"
	// RouteConnect covers the native Connect/gRPC procedure paths, which are
	// bounded by the service definition.
	RouteConnect = "/task.v1.TaskService/{method}"
)

// Route returns the templated label for a request path.
func Route(path string) string {
	switch path {
	case "/api/v1/tasks", "/tasks":
		return path
	case "/healthz", "/readyz", "/metrics", "/version",
		"/openapi.yaml", "/openapi.json", "/docs", "/docs/":
		return path
	}

	if rest, ok := strings.CutPrefix(path, "/api/v1/tasks/"); ok && isSingleSegment(rest) {
		return "/api/v1/tasks/{id}"
	}
	if rest, ok := strings.CutPrefix(path, "/tasks/"); ok && isSingleSegment(rest) {
		return "/tasks/{id}"
	}
	if rest, ok := strings.CutPrefix(path, "/task.v1.TaskService/"); ok && isSingleSegment(rest) {
		return RouteConnect
	}
	if strings.HasPrefix(path, "/debug/") {
		return "/debug/*"
	}
	return RouteOther
}

// IsLegacyRoute reports whether a path is on the unversioned surface kept for
// compatibility with the original specification.
func IsLegacyRoute(path string) bool {
	if path == "/tasks" {
		return true
	}
	rest, ok := strings.CutPrefix(path, "/tasks/")
	return ok && isSingleSegment(rest)
}

func isSingleSegment(s string) bool {
	return s != "" && !strings.Contains(s, "/")
}
