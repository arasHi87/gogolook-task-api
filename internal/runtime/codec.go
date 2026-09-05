package runtime

import (
	"errors"
	"fmt"

	"connectrpc.com/connect"
	"connectrpc.com/vanguard"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// marshalOptions is the JSON rendering used by every path that produces a
// response body, and it is shared deliberately.
//
// EmitDefaultValues is what puts "status": 0 on the wire. protojson omits zero
// values by default, so an incomplete task would come back with no status field
// at all — and the contract says status is an integer that is 0 or 1, not a
// field that disappears when it is 0. EmitUnpopulated would do the same job but
// also render unset message fields as null, which is noise.
//
// It has to be set in two places because vanguard only re-encodes a response
// when the annotation reshapes it (response_body: "task"); otherwise the
// handler's own JSON is passed straight through. Two codecs, one set of
// options, so the two paths cannot disagree about what a task looks like.
var marshalOptions = protojson.MarshalOptions{EmitDefaultValues: true}

// unmarshalOptions discards unknown fields so a client sending a field we have
// since removed is not broken by it.
var unmarshalOptions = protojson.UnmarshalOptions{DiscardUnknown: true}

// jsonCodec is vanguard's JSON codec with two changes: responses render default
// values, and a request body that fails to parse is an InvalidArgument rather
// than an unlabelled failure.
//
// Vanguard reports a decode error from the request-reading path as a plain
// error, and its response writer turns anything that is not already a
// *connect.Error into CodeUnknown — a 500. A client that sends `{` is not
// causing a server error; it is sending a bad request, and it deserves a 400
// that says so. Returning an error that already carries the code is enough,
// because the reporting path checks with errors.As before falling back.
type jsonCodec struct {
	*vanguard.JSONCodec
}

var (
	_ vanguard.Codec       = jsonCodec{}
	_ vanguard.StableCodec = jsonCodec{}
	_ vanguard.RESTCodec   = jsonCodec{}
)

// newJSONCodec is the codec factory handed to the transcoder.
func newJSONCodec(res vanguard.TypeResolver) vanguard.Codec {
	base := vanguard.NewJSONCodec(res)
	base.MarshalOptions = protojson.MarshalOptions{
		Resolver:          res,
		EmitDefaultValues: marshalOptions.EmitDefaultValues,
	}
	base.UnmarshalOptions = protojson.UnmarshalOptions{
		Resolver:       res,
		DiscardUnknown: unmarshalOptions.DiscardUnknown,
	}
	return jsonCodec{JSONCodec: base}
}

// connectJSONCodec renders the Connect handler's own JSON, which is what
// vanguard passes through unchanged for the responses it does not reshape.
type connectJSONCodec struct{}

var _ connect.Codec = connectJSONCodec{}

// Name is "json", which replaces connect-go's built-in codec of that name.
func (connectJSONCodec) Name() string { return "json" }

func (connectJSONCodec) Marshal(msg any) ([]byte, error) {
	m, ok := msg.(proto.Message)
	if !ok {
		return nil, fmt.Errorf("json codec cannot marshal %T", msg)
	}
	return marshalOptions.Marshal(m)
}

func (connectJSONCodec) Unmarshal(data []byte, msg any) error {
	m, ok := msg.(proto.Message)
	if !ok {
		return fmt.Errorf("json codec cannot unmarshal into %T", msg)
	}
	return asInvalidArgument(unmarshalOptions.Unmarshal(data, m))
}

// Unmarshal decodes a whole message body.
func (c jsonCodec) Unmarshal(data []byte, msg proto.Message) error {
	return asInvalidArgument(c.JSONCodec.Unmarshal(data, msg))
}

// UnmarshalField decodes a body that maps to a single field, which is what
// `body: "task"` in the HTTP annotation produces.
func (c jsonCodec) UnmarshalField(data []byte, msg proto.Message, field protoreflect.FieldDescriptor) error {
	return asInvalidArgument(c.JSONCodec.UnmarshalField(data, msg, field))
}

func asInvalidArgument(err error) error {
	if err == nil {
		return nil
	}
	// Already coded (by us, or by vanguard itself): leave it alone.
	var coded *connect.Error
	if errors.As(err, &coded) {
		return err
	}
	return connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("malformed request body: %w", err))
}
