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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/go-logr/zapr"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

func TestWithFields(t *testing.T) {
	baseErr := errors.New("listing Workloads failed")
	ancestorJob := Field{Key: "ancestorJob", Value: klog.KRef("ns", "parent")}

	cases := map[string]struct {
		err        error
		fields     []Field
		wantSame   bool
		wantFields []Field
	}{
		"nil error": {
			fields:   []Field{ancestorJob},
			wantSame: true,
		},
		"no fields": {
			err:      baseErr,
			wantSame: true,
		},
		"fields": {
			err:        baseErr,
			fields:     []Field{ancestorJob},
			wantFields: []Field{ancestorJob},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gotErr := WithFields(tc.err, tc.fields...)
			if tc.wantSame {
				if gotErr != tc.err {
					t.Errorf("WithFields() = %v, want the same error %v", gotErr, tc.err)
				}
				return
			}
			if !errors.Is(gotErr, tc.err) {
				t.Errorf("WithFields() = %v, want an error that wraps %v", gotErr, tc.err)
			}
			if gotErr.Error() != tc.err.Error() {
				t.Errorf("Unexpected error message %q, want %q", gotErr.Error(), tc.err.Error())
			}
			if diff := cmp.Diff(tc.wantFields, fieldsInErrorTree(gotErr)); diff != "" {
				t.Errorf("Unexpected fields (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestCustomLogProcessorWritesErrorFields(t *testing.T) {
	baseErr := errors.New("listing Workloads failed")
	ancestorJob := Field{Key: "ancestorJob", Value: klog.KRef("ns", "parent")}

	cases := map[string]struct {
		log       func(logr.Logger)
		wantEntry map[string]any
	}{
		"error without fields": {
			log: func(log logr.Logger) {
				log.Error(baseErr, "Reconciler error")
			},
			wantEntry: map[string]any{
				"msg":   "Reconciler error",
				"error": "listing Workloads failed",
			},
		},
		"fields of a wrapped error, with a logr.Marshaler value logged as a structure": {
			log: func(log logr.Logger) {
				log.Error(fmt.Errorf("getting unfinished Workload for ancestor job: %w", WithFields(baseErr, ancestorJob)), "Reconciler error")
			},
			wantEntry: map[string]any{
				"msg":         "Reconciler error",
				"error":       "getting unfinished Workload for ancestor job: listing Workloads failed",
				"ancestorJob": map[string]any{"name": "parent", "namespace": "ns"},
			},
		},
		"fields of all errors in the tree, with the outer error winning on duplicate keys": {
			log: func(log logr.Logger) {
				err := WithFields(
					errors.Join(
						WithFields(errors.New("first"), Field{Key: "workload", Value: "inner"}, Field{Key: "attempt", Value: 2}),
						WithFields(errors.New("second"), Field{Key: "clusterQueue", Value: "cq"}),
					),
					Field{Key: "workload", Value: "outer"},
				)
				log.Error(err, "Reconciler error")
			},
			wantEntry: map[string]any{
				"msg":          "Reconciler error",
				"error":        "first\nsecond",
				"workload":     "outer",
				"attempt":      float64(2),
				"clusterQueue": "cq",
			},
		},
		"key/value pairs of the log call win over error fields": {
			log: func(log logr.Logger) {
				err := WithFields(baseErr, Field{Key: "workload", Value: "from-error"}, Field{Key: "error", Value: "from-error"})
				log.Error(err, "Reconciler error", "workload", "from-call")
			},
			wantEntry: map[string]any{
				"msg":      "Reconciler error",
				"error":    "listing Workloads failed",
				"workload": "from-call",
			},
		},
		"keys of the logger context win over error fields": {
			log: func(log logr.Logger) {
				log.WithValues("workload", "from-context").Error(WithFields(baseErr, Field{Key: "workload", Value: "from-error"}), "Reconciler error")
			},
			// A repeated key would come last in the JSON and win in the decoded map.
			wantEntry: map[string]any{
				"msg":      "Reconciler error",
				"error":    "listing Workloads failed",
				"workload": "from-context",
			},
		},
		"fields of an error logged as a key/value pair": {
			log: func(log logr.Logger) {
				log.Info("Received an unretryable error", "error", WithFields(baseErr, ancestorJob))
			},
			wantEntry: map[string]any{
				"msg":         "Received an unretryable error",
				"error":       "listing Workloads failed",
				"ancestorJob": map[string]any{"name": "parent", "namespace": "ns"},
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gotEntry := logJSON(t, tc.log)
			if diff := cmp.Diff(tc.wantEntry, gotEntry); diff != "" {
				t.Errorf("Unexpected log entry (-want,+got):\n%s", diff)
			}
		})
	}
}

type queueName string

type panickingMarshaler struct{}

func (panickingMarshaler) MarshalLog() any {
	panic("boom")
}

func TestCustomLogProcessorWritesErrorFieldsLikeKeyValues(t *testing.T) {
	cases := map[string]struct {
		value any
	}{
		"string":                   {value: "cq"},
		"integer":                  {value: 2},
		"named string type":        {value: queueName("cq")},
		"nil":                      {value: nil},
		"fmt.Stringer":             {value: types.NamespacedName{Namespace: "ns", Name: "parent"}},
		"logr.Marshaler":           {value: klog.KRef("ns", "parent")},
		"panicking logr.Marshaler": {value: panickingMarshaler{}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := errors.New("listing Workloads failed")
			wantEntry := logJSON(t, func(log logr.Logger) {
				log.Error(err, "Reconciler error", "value", tc.value)
			})
			gotEntry := logJSON(t, func(log logr.Logger) {
				log.Error(WithFields(err, Field{Key: "value", Value: tc.value}), "Reconciler error")
			})
			if diff := cmp.Diff(wantEntry, gotEntry); diff != "" {
				t.Errorf("Unexpected log entry (-key/value pair,+error field):\n%s", diff)
			}
		})
	}
}

// logJSON logs with a zapr logger that uses CustomLogProcessor, as Kueue does,
// and returns the decoded JSON entry.
func logJSON(t *testing.T, log func(logr.Logger)) map[string]any {
	t.Helper()
	var buf bytes.Buffer
	encoder := zapcore.NewJSONEncoder(zapcore.EncoderConfig{MessageKey: "msg"})
	core := zapcore.NewCore(encoder, zapcore.AddSync(&buf), zapcore.DebugLevel)
	log(zapr.NewLogger(zap.New(NewCustomLogProcessor(core))))
	var entry map[string]any
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("Failed to decode the log entry %q: %v", buf.String(), err)
	}
	return entry
}
