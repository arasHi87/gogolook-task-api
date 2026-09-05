// Package api is the RPC handler, and only the RPC handler.
//
// Each method does four things: convert the request, call internal/task,
// convert the result, map the error. No business rules live here, and no
// transport wiring either — internal/runtime owns the routes, the codecs and
// the middleware.
//
// That leaves the domain testable without a transport, and the transport
// replaceable without touching the domain.
package api

import (
	"context"

	"connectrpc.com/connect"

	taskv1 "github.com/arasHi87/gogolook-task-api/gen/task/v1"
	"github.com/arasHi87/gogolook-task-api/gen/task/v1/taskv1connect"
	"github.com/arasHi87/gogolook-task-api/internal/apperr"
	"github.com/arasHi87/gogolook-task-api/internal/task"
)

// Server implements the generated TaskService handler.
type Server struct {
	svc *task.Service
}

// NewServer wires the handler over the domain service.
func NewServer(svc *task.Service) *Server { return &Server{svc: svc} }

var _ taskv1connect.TaskServiceHandler = (*Server)(nil)

// ListTasks returns a page of tasks, newest first.
func (s *Server) ListTasks(
	ctx context.Context,
	req *connect.Request[taskv1.ListTasksRequest],
) (*connect.Response[taskv1.ListTasksResponse], error) {
	msg := req.Msg

	page, err := s.svc.List(ctx, task.ListInput{
		Status:    statusFilter(msg.Status),
		PageSize:  int(msg.GetPageSize()),
		PageToken: msg.GetPageToken(),
	})
	if err != nil {
		return nil, toConnectError(ctx, err)
	}

	return connect.NewResponse(&taskv1.ListTasksResponse{
		Result:        toProtoList(page.Tasks),
		NextPageToken: page.Next.Encode(),
	}), nil
}

// CreateTask stores a new task.
func (s *Server) CreateTask(
	ctx context.Context,
	req *connect.Request[taskv1.CreateTaskRequest],
) (*connect.Response[taskv1.CreateTaskResponse], error) {
	in := req.Msg.GetTask()
	if in == nil {
		return nil, toConnectError(ctx, apperr.Field("task", "must be present"))
	}

	// id, timestamps and version on the request are ignored rather than
	// rejected: they are server-owned, and a client that round-trips a task it
	// read should not have to strip them.
	created, err := s.svc.Create(ctx, task.CreateInput{
		Name:   in.GetName(),
		Status: task.Status(in.GetStatus()),
	})
	if err != nil {
		return nil, toConnectError(ctx, err)
	}

	return connect.NewResponse(&taskv1.CreateTaskResponse{Task: toProto(created)}), nil
}

// UpdateTask replaces a task.
func (s *Server) UpdateTask(
	ctx context.Context,
	req *connect.Request[taskv1.UpdateTaskRequest],
) (*connect.Response[taskv1.UpdateTaskResponse], error) {
	id, err := parseID(req.Msg.GetId())
	if err != nil {
		return nil, toConnectError(ctx, err)
	}
	in := req.Msg.GetTask()
	if in == nil {
		return nil, toConnectError(ctx, apperr.Field("task", "must be present"))
	}

	// The path wins over the body's id. Two sources of truth for the same value
	// is a bug waiting to happen, and the URL is the one the router matched on.
	updated, err := s.svc.Update(ctx, task.UpdateInput{
		ID:              id,
		Name:            in.GetName(),
		Status:          task.Status(in.GetStatus()),
		ExpectedVersion: in.GetVersion(),
	})
	if err != nil {
		return nil, toConnectError(ctx, err)
	}

	return connect.NewResponse(&taskv1.UpdateTaskResponse{Task: toProto(updated)}), nil
}

// DeleteTask removes a task.
func (s *Server) DeleteTask(
	ctx context.Context,
	req *connect.Request[taskv1.DeleteTaskRequest],
) (*connect.Response[taskv1.DeleteTaskResponse], error) {
	id, err := parseID(req.Msg.GetId())
	if err != nil {
		return nil, toConnectError(ctx, err)
	}
	if err := s.svc.Delete(ctx, id); err != nil {
		return nil, toConnectError(ctx, err)
	}
	return connect.NewResponse(&taskv1.DeleteTaskResponse{}), nil
}
