package e2e

import (
	"net/http"
	"testing"

	"github.com/arasHi87/gogolook-task-api/test/harness"
)

// TestContractHoldsOnPostgres drives the assignment's four endpoints against
// the shipped binary on a real database.
//
// internal/runtime/contract_test.go already asserts this surface exhaustively,
// in-process and against the in-memory store. This is not a duplicate of it:
// it proves the same contract survives the parts that test cannot reach — the
// flag and environment merge that chose the postgres backend, the pool that
// had to connect before the first request, and the repository that talks SQL
// instead of a map. A backend swap that quietly changed a status code would
// pass there and fail here.
func TestContractHoldsOnPostgres(t *testing.T) {
	t.Parallel()

	// No worker: this is about the write path, and a consumer draining the
	// queue underneath it adds nothing but a moving target.
	sys := harness.Start(t, harness.WithoutWorker())

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
			api := sys.API.At(s.prefix)

			created := api.Create("write a harness", 0)
			if created.Status != 0 {
				t.Errorf("create: status %d, want 0", created.Status)
			}

			updated := api.Update(created.ID, "write a harness", 1)
			switch {
			case updated.ID != created.ID:
				t.Errorf("update: id %s, want %s", updated.ID, created.ID)
			case updated.Status != 1:
				t.Errorf("update: status %d, want 1", updated.Status)
			}

			found := false
			for _, task := range api.List().Result {
				if task.ID == created.ID {
					found = true
				}
			}
			if !found {
				t.Errorf("list: %s is missing", created.ID)
			}

			api.Delete(created.ID)

			// Deleting an unknown id is a 404, not a silent success — the one
			// behaviour a naive DELETE implementation gets wrong.
			api.Do(http.MethodDelete, "/tasks/"+created.ID, "").Status(http.StatusNotFound)

			// status is [0, 1]; 2 is not a task that is twice as done.
			api.Post("/tasks", `{"name":"nope","status":2}`).
				Status(http.StatusBadRequest).
				BodyContains("must be in list [0, 1]")

			// The unversioned surface is supported, and says it is on the way
			// out. The canonical one must not carry that header at all.
			list := api.Get("/tasks").Status(http.StatusOK)
			if s.deprecated {
				list.HasHeader("Deprecation", "true")
				if list.Header("Sunset") == "" {
					t.Error("Deprecation is set but Sunset is not; a deprecation with no date is a rumour")
				}
			} else {
				list.NoHeader("Deprecation")
			}
		})
	}

	// Both surfaces created one and deleted it.
	sys.Tasks.Count(0)
}
