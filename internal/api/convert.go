package api

import (
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	taskv1 "github.com/arasHi87/gogolook-task-api/gen/task/v1"
	"github.com/arasHi87/gogolook-task-api/internal/apperr"
	"github.com/arasHi87/gogolook-task-api/internal/task"
)

// toProto renders a domain task on the wire.
func toProto(t *task.Task) *taskv1.Task {
	if t == nil {
		return nil
	}
	return &taskv1.Task{
		Id:        t.ID.String(),
		Name:      t.Name,
		Status:    int32(t.Status),
		CreatedAt: timestamppb.New(t.CreatedAt),
		UpdatedAt: timestamppb.New(t.UpdatedAt),
		Version:   t.Version,
	}
}

func toProtoList(ts []*task.Task) []*taskv1.Task {
	out := make([]*taskv1.Task, 0, len(ts))
	for _, t := range ts {
		out = append(out, toProto(t))
	}
	return out
}

// parseID turns a path parameter into an id.
//
// protovalidate already rejects a malformed uuid at the edge, but this is
// reached directly by unit tests and by any future caller, so it produces the
// same field-scoped error rather than trusting that something upstream checked.
func parseID(raw string) (uuid.UUID, error) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, apperr.Field("id", "must be a UUID")
	}
	return id, nil
}

// statusFilter converts an optional wire status into an optional domain one.
func statusFilter(s *int32) *task.Status {
	if s == nil {
		return nil
	}
	v := task.Status(*s)
	return &v
}
