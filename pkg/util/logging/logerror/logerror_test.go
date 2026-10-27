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

package logerror

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
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
			if diff := cmp.Diff(tc.wantFields, Fields(gotErr)); diff != "" {
				t.Errorf("Unexpected fields (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestWithValues(t *testing.T) {
	baseErr := errors.New("listing Workloads failed")
	ancestorJobRef := klog.KRef("ns", "parent")

	cases := map[string]struct {
		err           error
		keysAndValues []any
		wantSame      bool
		wantFields    []Field
	}{
		"nil error": {
			keysAndValues: []any{"ancestorJob", ancestorJobRef},
			wantSame:      true,
		},
		"no key/value pairs": {
			err:      baseErr,
			wantSame: true,
		},
		"key/value pairs": {
			err:           baseErr,
			keysAndValues: []any{"ancestorJob", ancestorJobRef, "attempt", 2},
			wantFields:    []Field{{Key: "ancestorJob", Value: ancestorJobRef}, {Key: "attempt", Value: 2}},
		},
		"key without a value is dropped": {
			err:           baseErr,
			keysAndValues: []any{"ancestorJob", ancestorJobRef, "attempt"},
			wantFields:    []Field{{Key: "ancestorJob", Value: ancestorJobRef}},
		},
		"pairs from a key that is not a string are dropped": {
			err:           baseErr,
			keysAndValues: []any{"ancestorJob", ancestorJobRef, 2, "attempt", "workload", "ns/wl"},
			wantFields:    []Field{{Key: "ancestorJob", Value: ancestorJobRef}},
		},
		"no valid pairs": {
			err:           baseErr,
			keysAndValues: []any{2, "attempt"},
			wantSame:      true,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gotErr := WithValues(tc.err, tc.keysAndValues...)
			if tc.wantSame {
				if gotErr != tc.err {
					t.Errorf("WithValues() = %v, want the same error %v", gotErr, tc.err)
				}
				return
			}
			if !errors.Is(gotErr, tc.err) {
				t.Errorf("WithValues() = %v, want an error that wraps %v", gotErr, tc.err)
			}
			if diff := cmp.Diff(tc.wantFields, Fields(gotErr)); diff != "" {
				t.Errorf("Unexpected fields (-want,+got):\n%s", diff)
			}
		})
	}
}

func TestFields(t *testing.T) {
	workload := Field{Key: "workload", Value: "ns/wl"}
	clusterQueue := Field{Key: "clusterQueue", Value: "cq"}
	attempt := Field{Key: "attempt", Value: 2}

	cases := map[string]struct {
		err        error
		wantFields []Field
	}{
		"nil error": {},
		"error without fields": {
			err: errors.New("failed"),
		},
		"error wrapped with fmt.Errorf": {
			err:        fmt.Errorf("getting Workload: %w", WithFields(errors.New("failed"), workload)),
			wantFields: []Field{workload},
		},
		"nested errors, outer error first": {
			err:        WithFields(fmt.Errorf("retrying: %w", WithFields(errors.New("failed"), attempt)), workload),
			wantFields: []Field{workload, attempt},
		},
		"joined errors": {
			err:        errors.Join(WithFields(errors.New("first"), workload), WithFields(errors.New("second"), clusterQueue)),
			wantFields: []Field{workload, clusterQueue},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if diff := cmp.Diff(tc.wantFields, Fields(tc.err)); diff != "" {
				t.Errorf("Unexpected fields (-want,+got):\n%s", diff)
			}
		})
	}
}
