package api_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	taskv1 "github.com/arasHi87/gogolook-task-api/gen/task/v1"
	"github.com/arasHi87/gogolook-task-api/internal/api"
	"github.com/arasHi87/gogolook-task-api/internal/task"
	"github.com/arasHi87/gogolook-task-api/internal/task/memrepo"
)

// newServer returns the handler over an empty in-memory store.
//
// These tests call the handler directly, with no HTTP and no transcoding,
// which is the point of the package being only the handler: the adapter's
// behaviour is observable without standing a server up. The wire contract is
// tested against the real mux in internal/runtime.
func newServer(t *testing.T) *api.Server {
	t.Helper()
	return api.NewServer(task.NewService(memrepo.New()))
}

func create(t *testing.T, s *api.Server, name string, status int32) *taskv1.Task {
	t.Helper()
	res, err := s.CreateTask(context.Background(), connect.NewRequest(&taskv1.CreateTaskRequest{
		Task: &taskv1.Task{Name: name, Status: status},
	}))
	if err != nil {
		t.Fatalf("CreateTask(%q): %v", name, err)
	}
	return res.Msg.GetTask()
}

// wantCode asserts the handler returned a connect error with a given code.
func wantCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("got nil error, want %v", want)
	}
	if got := connect.CodeOf(err); got != want {
		t.Errorf("code = %v, want %v (%v)", got, want, err)
	}
}

func TestCreateAssignsServerOwnedFields(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	got := create(t, s, "buy milk", 0)
	if got.GetId() == "" {
		t.Error("no id was assigned")
	}
	if got.GetVersion() != 1 {
		t.Errorf("version = %d, want 1", got.GetVersion())
	}
	if got.GetCreatedAt() == nil || got.GetUpdatedAt() == nil {
		t.Error("timestamps were not set")
	}
}

// id, timestamps and version on a create request are ignored rather than
// rejected: they are server-owned, and a client round-tripping a task it read
// should not have to strip them.
func TestCreateIgnoresServerOwnedFieldsInTheRequest(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	res, err := s.CreateTask(context.Background(), connect.NewRequest(&taskv1.CreateTaskRequest{
		Task: &taskv1.Task{
			Id:      "1f0c1b2e-0000-4000-8000-000000000000",
			Name:    "spoofed",
			Status:  0,
			Version: 99,
		},
	}))
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got := res.Msg.GetTask()
	if got.GetId() == "1f0c1b2e-0000-4000-8000-000000000000" {
		t.Error("the client's id was accepted; ids are server-assigned")
	}
	if got.GetVersion() != 1 {
		t.Errorf("version = %d, want 1: the client's version was accepted", got.GetVersion())
	}
}

func TestCreateRejectsAMissingTask(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	_, err := s.CreateTask(context.Background(), connect.NewRequest(&taskv1.CreateTaskRequest{}))
	wantCode(t, err, connect.CodeInvalidArgument)
	if !strings.Contains(err.Error(), "task") {
		t.Errorf("error = %v, want it to name the missing field", err)
	}
}

// The domain validates on its own account, not because protovalidate happened
// to run first. Calling the handler directly is what proves it.
func TestHandlerValidatesWithoutTheInterceptor(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	cases := map[string]*taskv1.Task{
		"empty name":    {Name: ""},
		"blank name":    {Name: "   "},
		"name too long": {Name: strings.Repeat("x", 256)},
		"bad status":    {Name: "ok", Status: 2},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := s.CreateTask(context.Background(),
				connect.NewRequest(&taskv1.CreateTaskRequest{Task: in}))
			wantCode(t, err, connect.CodeInvalidArgument)
		})
	}
}

// The path wins over the body's id: two sources of truth for one value is a
// bug waiting to happen, and the URL is what the router matched on.
func TestUpdateTakesTheIDFromThePath(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	created := create(t, s, "original", 0)

	res, err := s.UpdateTask(context.Background(), connect.NewRequest(&taskv1.UpdateTaskRequest{
		Id: created.GetId(),
		Task: &taskv1.Task{
			Id:     "1f0c1b2e-0000-4000-8000-000000000000", // ignored
			Name:   "replaced",
			Status: 1,
		},
	}))
	if err != nil {
		t.Fatalf("UpdateTask: %v", err)
	}

	got := res.Msg.GetTask()
	if got.GetId() != created.GetId() {
		t.Errorf("id = %q, want the path's %q", got.GetId(), created.GetId())
	}
	if got.GetName() != "replaced" || got.GetStatus() != 1 {
		t.Errorf("got %v, want the replacement values", got)
	}
	if got.GetVersion() != 2 {
		t.Errorf("version = %d, want 2", got.GetVersion())
	}
}

// A version in the body makes the write conditional. Omitting it is
// last-write-wins, which is what a client that never read the task wants.
func TestUpdateVersionIsOptimisticConcurrency(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	created := create(t, s, "original", 0)

	replace := func(version int64) error {
		_, err := s.UpdateTask(context.Background(), connect.NewRequest(&taskv1.UpdateTaskRequest{
			Id:   created.GetId(),
			Task: &taskv1.Task{Name: "mine", Status: 1, Version: version},
		}))
		return err
	}

	if err := replace(0); err != nil {
		t.Fatalf("unconditional update: %v", err)
	}
	wantCode(t, replace(created.GetVersion()), connect.CodeAborted)
	if err := replace(0); err != nil {
		t.Errorf("unconditional update after a conflict: %v", err)
	}
}

func TestUnknownIDIsNotFound(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	missing := "1f0c1b2e-0000-4000-8000-000000000000"

	_, err := s.UpdateTask(context.Background(), connect.NewRequest(&taskv1.UpdateTaskRequest{
		Id: missing, Task: &taskv1.Task{Name: "x"},
	}))
	wantCode(t, err, connect.CodeNotFound)

	_, err = s.DeleteTask(context.Background(), connect.NewRequest(&taskv1.DeleteTaskRequest{Id: missing}))
	wantCode(t, err, connect.CodeNotFound)
}

func TestMalformedIDIsInvalidArgument(t *testing.T) {
	t.Parallel()
	s := newServer(t)

	_, err := s.DeleteTask(context.Background(), connect.NewRequest(&taskv1.DeleteTaskRequest{Id: "nope"}))
	wantCode(t, err, connect.CodeInvalidArgument)
}

func TestListFiltersPaginatesAndOrders(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	for i := range 5 {
		create(t, s, "task", int32(i%2))
	}

	t.Run("status filter", func(t *testing.T) {
		want := int32(1)
		res, err := s.ListTasks(context.Background(),
			connect.NewRequest(&taskv1.ListTasksRequest{Status: &want}))
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		if len(res.Msg.GetResult()) != 2 {
			t.Fatalf("got %d completed tasks, want 2", len(res.Msg.GetResult()))
		}
	})

	t.Run("page token", func(t *testing.T) {
		res, err := s.ListTasks(context.Background(),
			connect.NewRequest(&taskv1.ListTasksRequest{PageSize: 2}))
		if err != nil {
			t.Fatalf("ListTasks: %v", err)
		}
		if len(res.Msg.GetResult()) != 2 || res.Msg.GetNextPageToken() == "" {
			t.Fatalf("got %d tasks and token %q, want 2 and a cursor",
				len(res.Msg.GetResult()), res.Msg.GetNextPageToken())
		}
	})

	t.Run("page size above the maximum is refused", func(t *testing.T) {
		_, err := s.ListTasks(context.Background(),
			connect.NewRequest(&taskv1.ListTasksRequest{PageSize: 1000}))
		wantCode(t, err, connect.CodeInvalidArgument)
	})

	t.Run("a forged page token is refused", func(t *testing.T) {
		_, err := s.ListTasks(context.Background(),
			connect.NewRequest(&taskv1.ListTasksRequest{PageToken: "not-a-real-token"}))
		wantCode(t, err, connect.CodeInvalidArgument)
	})
}

func TestDeleteRemovesTheTask(t *testing.T) {
	t.Parallel()
	s := newServer(t)
	created := create(t, s, "temporary", 0)

	if _, err := s.DeleteTask(context.Background(),
		connect.NewRequest(&taskv1.DeleteTaskRequest{Id: created.GetId()})); err != nil {
		t.Fatalf("DeleteTask: %v", err)
	}

	res, err := s.ListTasks(context.Background(), connect.NewRequest(&taskv1.ListTasksRequest{}))
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(res.Msg.GetResult()) != 0 {
		t.Errorf("got %d tasks after delete, want 0", len(res.Msg.GetResult()))
	}
}
