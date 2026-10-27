// Adapted from github.com/moby/moby errdefs (Apache License 2.0,
// Copyright Docker, Inc.; see https://github.com/moby/moby/blob/master/LICENSE).

// Package errdefs classifies errors by the kind of failure, so the HTTP layer
// can answer with the status the Docker daemon uses for it. An error is of a
// kind when it, or an error it wraps, implements that kind's marker method.
package errdefs

import "errors"

type (
	errNotFound         struct{ error }
	errInvalidParameter struct{ error }
	errConflict         struct{ error }
	errForbidden        struct{ error }
	errNotImplemented   struct{ error }
	errUnavailable      struct{ error }
	errNotModified      struct{ error }
)

func (errNotFound) NotFound()                 {}
func (errInvalidParameter) InvalidParameter() {}
func (errConflict) Conflict()                 {}
func (errForbidden) Forbidden()               {}
func (errNotImplemented) NotImplemented()     {}
func (errUnavailable) Unavailable()           {}
func (errNotModified) NotModified()           {}

func (e errNotFound) Unwrap() error         { return e.error }
func (e errInvalidParameter) Unwrap() error { return e.error }
func (e errConflict) Unwrap() error         { return e.error }
func (e errForbidden) Unwrap() error        { return e.error }
func (e errNotImplemented) Unwrap() error   { return e.error }
func (e errUnavailable) Unwrap() error      { return e.error }
func (e errNotModified) Unwrap() error      { return e.error }

// NotFound marks err as a missing object (404).
func NotFound(err error) error {
	return wrap(err, IsNotFound, func(e error) error { return errNotFound{e} })
}

// InvalidParameter marks err as invalid input (400).
func InvalidParameter(err error) error {
	return wrap(err, IsInvalidParameter, func(e error) error { return errInvalidParameter{e} })
}

// Conflict marks err as a conflict with existing state (409).
func Conflict(err error) error {
	return wrap(err, IsConflict, func(e error) error { return errConflict{e} })
}

// Forbidden marks err as a forbidden operation (403).
func Forbidden(err error) error {
	return wrap(err, IsForbidden, func(e error) error { return errForbidden{e} })
}

// NotImplemented marks err as an unsupported operation (501).
func NotImplemented(err error) error {
	return wrap(err, IsNotImplemented, func(e error) error { return errNotImplemented{e} })
}

// Unavailable marks err as a temporarily unavailable dependency (503).
func Unavailable(err error) error {
	return wrap(err, IsUnavailable, func(e error) error { return errUnavailable{e} })
}

// NotModified marks err as a request that changed nothing (304): starting
// a running container, stopping a stopped one.
func NotModified(err error) error {
	return wrap(err, IsNotModified, func(e error) error { return errNotModified{e} })
}

func wrap(err error, is func(error) bool, mark func(error) error) error {
	if err == nil || is(err) {
		return err
	}
	return mark(err)
}

// IsNotFound reports whether err is of the NotFound kind.
func IsNotFound(err error) bool { return isKind[interface{ NotFound() }](err) }

// IsInvalidParameter reports whether err is of the InvalidParameter kind.
func IsInvalidParameter(err error) bool { return isKind[interface{ InvalidParameter() }](err) }

// IsConflict reports whether err is of the Conflict kind.
func IsConflict(err error) bool { return isKind[interface{ Conflict() }](err) }

// IsForbidden reports whether err is of the Forbidden kind.
func IsForbidden(err error) bool { return isKind[interface{ Forbidden() }](err) }

// IsNotImplemented reports whether err is of the NotImplemented kind.
func IsNotImplemented(err error) bool { return isKind[interface{ NotImplemented() }](err) }

// IsUnavailable reports whether err is of the Unavailable kind.
func IsUnavailable(err error) bool { return isKind[interface{ Unavailable() }](err) }

// IsNotModified reports whether err is of the NotModified kind.
func IsNotModified(err error) bool { return isKind[interface{ NotModified() }](err) }

func isKind[T any](err error) bool {
	var target T
	return errors.As(err, &target)
}
