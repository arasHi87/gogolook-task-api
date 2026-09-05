package api_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	taskv1 "github.com/arasHi87/gogolook-task-api/gen/task/v1"
	"github.com/arasHi87/gogolook-task-api/internal/api"
	"github.com/arasHi87/gogolook-task-api/internal/apperr"
	"github.com/arasHi87/gogolook-task-api/internal/task"
	"github.com/arasHi87/gogolook-task-api/internal/task/memrepo"
)

// failingRepo returns a fixed error from every method, so the mapping from a
// domain error to a transport code can be exercised without contriving the
// state that would normally produce it.
type failingRepo struct{ err error }

func (r failingRepo) Create(context.Context, *task.Task) (*task.Task, error) { return nil, r.err }
func (r failingRepo) Get(context.Context, uuid.UUID) (*task.Task, error)     { return nil, r.err }
func (r failingRepo) Delete(context.Context, uuid.UUID) error                { return r.err }
func (r failingRepo) Update(context.Context, *task.Task, int64) (*task.Task, error) {
	return nil, r.err
}
func (r failingRepo) List(context.Context, task.ListQuery) (task.Page, error) {
	return task.Page{}, r.err
}

// The mapping table, asserted end to end through the handler. It is the one
// place a domain error becomes a transport code, which is what keeps a storage
// error from ever reaching a client as itself.
func TestErrorMapping(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		err  error
		want connect.Code
	}{
		"not found":            {task.ErrNotFound, connect.CodeNotFound},
		"invalid argument":     {task.ErrInvalidArgument, connect.CodeInvalidArgument},
		"field-scoped invalid": {apperr.Field("name", "too short"), connect.CodeInvalidArgument},
		"conflict":             {task.ErrConflict, connect.CodeAborted},
		"canceled":             {context.Canceled, connect.CodeCanceled},
		"deadline":             {context.DeadlineExceeded, connect.CodeDeadlineExceeded},
		"anything else":        {errors.New("disk on fire"), connect.CodeInternal},

		// Wrapping must not lose the classification: the service wraps every
		// repository error with context before returning it.
		"wrapped not found": {fmt.Errorf("layer: %w", task.ErrNotFound), connect.CodeNotFound},
		"twice wrapped":     {fmt.Errorf("a: %w", fmt.Errorf("b: %w", task.ErrConflict)), connect.CodeAborted},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := api.NewServer(task.NewService(failingRepo{err: tc.err}))

			_, err := s.ListTasks(context.Background(), connect.NewRequest(&taskv1.ListTasksRequest{}))
			if err == nil {
				t.Fatalf("got nil error, want %v", tc.want)
			}
			if got := connect.CodeOf(err); got != tc.want {
				t.Errorf("code = %v, want %v (%v)", got, tc.want, err)
			}
		})
	}
}

// An unrecognised error returns a bare 500. Its text goes to the log, never to
// the client: echoing an internal message is an information leak and, more
// often, a confusing non-answer.
func TestInternalErrorsAreNotEchoed(t *testing.T) {
	t.Parallel()

	secret := "pq: password authentication failed for user \"taskapi\""
	s := api.NewServer(task.NewService(failingRepo{err: errors.New(secret)}))

	_, err := s.ListTasks(context.Background(), connect.NewRequest(&taskv1.ListTasksRequest{}))
	if err == nil {
		t.Fatal("got nil error")
	}
	if strings.Contains(err.Error(), "password") {
		t.Errorf("the internal error leaked to the client: %v", err)
	}
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Errorf("code = %v, want Internal", connect.CodeOf(err))
	}
}

// The caller's own input described back to them is safe to return, and it is
// what lets a client fix the request without guessing.
func TestInvalidArgumentNamesTheField(t *testing.T) {
	t.Parallel()
	s := api.NewServer(task.NewService(memrepo.New()))

	_, err := s.CreateTask(context.Background(), connect.NewRequest(&taskv1.CreateTaskRequest{
		Task: &taskv1.Task{Name: "", Status: 0},
	}))
	if err == nil {
		t.Fatal("got nil error")
	}
	if !strings.Contains(err.Error(), "name") {
		t.Errorf("error = %v, want it to name the offending field", err)
	}
}
