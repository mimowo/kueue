/*
Copyright The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package logerror attaches key/value pairs to errors, so that they are logged as
// structured fields wherever the errors are logged.
package logerror

import "slices"

// Field is a key/value pair that WithFields attaches to an error.
type Field struct {
	// Key is the log key, such as "ancestorJob".
	Key string
	// Value is logged like a logr value: a logr.Marshaler, such as
	// klog.ObjectRef, is logged as the result of MarshalLog.
	Value any
}

// WithFields returns err with fields attached for logging. Wherever the error is
// logged, such as in the controller-runtime "Reconciler error" entry,
// logging.CustomLogProcessor adds the fields as key/value pairs.
//
// The fields appear only in logs, so the unchanged message must make sense on its
// own, such as in events and conditions. On key collisions, the logger's own
// key/value pairs win, and an outer error wins over the errors it wraps.
// WithFields returns nil if err is nil, and err if fields is empty.
func WithFields(err error, fields ...Field) error {
	if err == nil || len(fields) == 0 {
		return err
	}
	return &errorWithFields{err: err, fields: slices.Clone(fields)}
}

// WithValues is like WithFields, but takes alternating keys and values, like logr.
// Like zapr, it drops a key without a value, and stops at a key that is not a string.
func WithValues(err error, keysAndValues ...any) error {
	if err == nil || len(keysAndValues) == 0 {
		return err
	}
	fields := make([]Field, 0, len(keysAndValues)/2)
	for i := 0; i+1 < len(keysAndValues); i += 2 {
		key, ok := keysAndValues[i].(string)
		if !ok {
			break
		}
		fields = append(fields, Field{Key: key, Value: keysAndValues[i+1]})
	}
	return WithFields(err, fields...)
}

// Fields returns the fields attached to err and the errors it wraps, outer errors
// first. It walks the tree like errors.As.
func Fields(err error) []Field {
	switch e := err.(type) {
	case *errorWithFields:
		return slices.Concat(e.fields, Fields(e.err))
	case interface{ Unwrap() error }:
		return Fields(e.Unwrap())
	case interface{ Unwrap() []error }:
		var fields []Field
		for _, wrapped := range e.Unwrap() {
			fields = append(fields, Fields(wrapped)...)
		}
		return fields
	}
	return nil
}

type errorWithFields struct {
	err    error
	fields []Field
}

func (e *errorWithFields) Error() string {
	return e.err.Error()
}

func (e *errorWithFields) Unwrap() error {
	return e.err
}
