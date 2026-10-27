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

	"sigs.k8s.io/kueue/pkg/util/logging/logerror"
)

// appendErrorFields returns fields with the logerror fields of the logged errors
// added, except those whose keys are already in fields or contextKeys.
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
		for _, errField := range logerror.Fields(err) {
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

func hasKey(fields []zapcore.Field, key string) bool {
	return slices.ContainsFunc(fields, func(f zapcore.Field) bool {
		return f.Key == key
	})
}

// zapField converts f like zapr converts a key/value pair, so a logr.Marshaler,
// such as klog.ObjectRef, is logged as the result of MarshalLog. Unlike zapr, it
// does not resolve slog.LogValuer.
func zapField(f logerror.Field) (field zapcore.Field) {
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
