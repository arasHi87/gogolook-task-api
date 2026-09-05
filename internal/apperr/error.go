package apperr

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Error is a classified error.
//
// It separates what a client may be told from what only the log may see: Msg
// is client-safe by construction, Err is the cause and stays internal. That
// split is why an unclassified failure can return a bare 500 without also
// throwing away the reason it happened.
type Error struct {
	// Kind is the classification every transport maps from.
	Kind Kind

	// Field names the offending input, for Invalid. It is echoed to the
	// client, because it is the client's own input being described back.
	Field string

	// Msg is safe to return to a client. Empty means the Kind's default.
	Msg string

	// Err is the cause. It is never shown to a client.
	Err error
}

// Error renders the whole chain, for logs.
func (e *Error) Error() string {
	var b strings.Builder
	if e.Field != "" {
		b.WriteString(e.Field)
		b.WriteString(": ")
	}
	if e.Msg != "" {
		b.WriteString(e.Msg)
	} else {
		b.WriteString(e.Kind.message())
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

// Unwrap exposes the cause, so errors.Is and errors.As reach through.
func (e *Error) Unwrap() error { return e.Err }

// Is matches on Kind, which is what makes a package sentinel work as a
// classification test:
//
//	errors.Is(err, task.ErrNotFound)   // true for any NotFound error
//
// Matching on kind rather than identity is deliberate. The question a caller
// is asking is "is this a not-found?", and a shared vocabulary is worth little
// if every package needs its own sentinel to test against. Where identity
// genuinely matters, compare the concrete value.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.Kind == e.Kind
}

// New returns a classified error with a client-safe message.
func New(kind Kind, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...)}
}

// Wrap classifies an existing error and keeps it in the chain.
//
// The message is client-safe; err is not, and is never rendered to a client.
func Wrap(kind Kind, err error, format string, args ...any) *Error {
	return &Error{Kind: kind, Msg: fmt.Sprintf(format, args...), Err: err}
}

// Field returns an Invalid error naming the input that was wrong.
//
// Naming the field rather than writing a sentence is what lets a client fix
// the request without guessing which of five values we objected to.
func Field(name, format string, args ...any) *Error {
	return &Error{Kind: Invalid, Field: name, Msg: fmt.Sprintf(format, args...)}
}

// KindOf reports how err is classified, walking the chain.
//
// Anything unclassified is Internal, which is the safe answer: a new error
// that nobody thought about gets a 500 and a log line rather than a status
// that happens to look deliberate.
func KindOf(err error) Kind {
	if err == nil {
		return Internal
	}

	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}

	// The standard library's own errors cannot carry a Kind, so they are
	// classified here rather than at every call site that might see one.
	switch {
	case errors.Is(err, context.Canceled):
		return Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return Timeout
	}
	return Internal
}

// Is reports whether err is classified as kind.
func Is(err error, kind Kind) bool { return KindOf(err) == kind }

// FieldOf returns the input an Invalid error named, or "".
func FieldOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Field
	}
	return ""
}

// Message is what a client may be told.
//
// An unclassified error yields the generic internal message: its text may name
// a host, a query or a credential, and none of that belongs in a response.
func Message(err error) string {
	kind := KindOf(err)
	if kind == Internal {
		return Internal.message()
	}

	var e *Error
	if !errors.As(err, &e) {
		return kind.message()
	}

	msg := e.Msg
	if msg == "" {
		msg = e.Kind.message()
	}
	if e.Field != "" {
		return e.Field + ": " + msg
	}
	return msg
}

// Retryable reports whether err is worth trying again unchanged.
func Retryable(err error) bool { return KindOf(err).Retryable() }
