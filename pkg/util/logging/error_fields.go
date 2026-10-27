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

package logging

import (
	"fmt"
	"slices"

	"github.com/go-logr/logr"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

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
// CustomLogProcessor adds the fields as key/value pairs.
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

// appendErrorFields returns fields with the Fields of the logged errors added,
// except those whose keys are already in fields or contextKeys.
func appendErrorFields(fields []zapcore.Field, contextKeys []string) []zapcore.Field {
	var extra []zapcore.Field
	keyTaken := func(key string) bool {
		return slices.Contains(contextKeys, key) || hasKey(fields, key) || hasKey(extra, key)
	}
	for _, field := range fields {
		err, isError := field.Interface.(error)
		if field.Type != zapcore.ErrorType || !isError {
			continue
		}
		for _, errField := range fieldsInErrorTree(err) {
			if !keyTaken(errField.Key) {
				extra = append(extra, zapField(errField))
			}
		}
	}
	if len(extra) == 0 {
		return fields
	}
	// zap passes the same slice to every core, so build a new one.
	return slices.Concat(fields, extra)
}

// fieldsInErrorTree returns the Fields attached to err and the errors it wraps,
// outer errors first. It walks the tree like errors.As.
func fieldsInErrorTree(err error) []Field {
	switch e := err.(type) {
	case *errorWithFields:
		return slices.Concat(e.fields, fieldsInErrorTree(e.err))
	case interface{ Unwrap() error }:
		return fieldsInErrorTree(e.Unwrap())
	case interface{ Unwrap() []error }:
		var fields []Field
		for _, wrapped := range e.Unwrap() {
			fields = append(fields, fieldsInErrorTree(wrapped)...)
		}
		return fields
	}
	return nil
}

func hasKey(fields []zapcore.Field, key string) bool {
	return slices.ContainsFunc(fields, func(f zapcore.Field) bool {
		return f.Key == key
	})
}

// zapField converts f like zapr converts a key/value pair, so a logr.Marshaler,
// such as klog.ObjectRef, is logged as the result of MarshalLog. Unlike zapr, it
// does not resolve slog.LogValuer.
func zapField(f Field) (field zapcore.Field) {
	marshaler, ok := f.Value.(logr.Marshaler)
	if !ok {
		return zap.Any(f.Key, f.Value)
	}
	defer func() {
		if r := recover(); r != nil {
			field = zap.String(f.Key+"Error", fmt.Sprintf("PANIC=%s", r))
		}
	}()
	return zap.Any(f.Key, marshaler.MarshalLog())
}
