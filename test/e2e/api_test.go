package e2e

import (
	"net/http"
	"testing"
)

// TestContractHoldsOnPostgres drives the assignment's four endpoints against
// the shipped binary on a real database.
//
// internal/runtime/contract_test.go already asserts this surface exhaustively,
// in-process and against the in-memory store. This one is not a duplicate of
// it: it proves the same contract survives the parts that test cannot reach —
// the flag and environment merge that chose the postgres backend, the pool
// that had to connect before the first request, and the repository that talks
// SQL instead of a map. A backend swap that quietly changed a status code
// would pass there and fail here.
func TestContractHoldsOnPostgres(t *testing.T) {
	t.Parallel()

	// No worker: this test is about the write path, and a consumer draining
	// the queue underneath it adds nothing but a moving target.
	h := start(t, withoutWorker())

	surfaces := []struct {
		name       string
		prefix     string
		deprecated bool
	}{
		{name: "canonical", prefix: "/api/v1", deprecated: false},
		{name: "unversioned", prefix: "", deprecated: true},
	}

	for _, s := range surfaces {
		t.Run(s.name, func(t *testing.T) {
			api := h.at(s.prefix)

			created := api.create("write a harness", 0)
			if created.Status != 0 {
				t.Errorf("create: status %d, want 0", created.Status)
			}

			updated := api.update(created.ID, "write a harness", 1)
			switch {
			case updated.ID != created.ID:
				t.Errorf("update: id %s, want %s", updated.ID, created.ID)
			case updated.Status != 1:
				t.Errorf("update: status %d, want 1", updated.Status)
			}

			found := false
			for _, task := range api.list().Result {
				if task.ID == created.ID {
					found = true
				}
			}
			if !found {
				t.Errorf("list: %s is missing", created.ID)
			}

			api.remove(created.ID)

			// Deleting an unknown id is a 404, not a silent success — the one
			// behaviour a naive DELETE implementation gets wrong.
			if resp, body := api.do(http.MethodDelete, "/tasks/"+created.ID, nil); resp.StatusCode != http.StatusNotFound {
				t.Errorf("second delete: status %d, want 404: %s", resp.StatusCode, body)
			}

			// status is [0, 1]; 2 is not a task that is twice as done.
			if resp, body := api.do(http.MethodPost, "/tasks", `{"name":"nope","status":2}`); resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status=2: status %d, want 400: %s", resp.StatusCode, body)
			}

			// The unversioned surface is supported, and says it is on the way
			// out. The canonical one must not carry that header at all.
			resp, _ := api.do(http.MethodGet, "/tasks", nil)
			got := resp.Header.Get("Deprecation")
			if want := boolHeader(s.deprecated); got != want {
				t.Errorf("Deprecation header %q, want %q", got, want)
			}
			if s.deprecated && resp.Header.Get("Sunset") == "" {
				t.Error("Deprecation is set but Sunset is not; a deprecation with no date is a rumour")
			}
		})
	}

	if n := h.taskCount(); n != 0 {
		t.Errorf("%d tasks survived; both surfaces created one and deleted it", n)
	}
}

func boolHeader(set bool) string {
	if set {
		return "true"
	}
	return ""
}
