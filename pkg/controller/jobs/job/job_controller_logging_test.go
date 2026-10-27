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

package job

import (
	"context"
	"errors"
	"testing"

	"github.com/go-logr/zapr"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"
	"sigs.k8s.io/kueue/pkg/features"
	utillogging "sigs.k8s.io/kueue/pkg/util/logging"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingjob "sigs.k8s.io/kueue/pkg/util/testingjobs/job"
)

func TestReconcilerAttachesAncestorJobToError(t *testing.T) {
	features.SetFeatureGateDuringTest(t, features.SkipChildJobSuspension, false)
	ctx, _ := utiltesting.ContextWithLog(t)
	integrationManager := newTestIntegrationManager(t)
	t.Cleanup(integrationManager.EnableIntegrationsForTest(t, FrameworkName))

	parentJob := utiltestingjob.MakeJob("parent", "ns").UID("parent").Queue("queue").Obj()
	childJob := utiltestingjob.MakeJob("child", "ns").OwnerReference(parentJob.Name, gvk).Suspend(false).Obj()

	errInjected := errors.New("injected error")
	clientBuilder := utiltesting.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, isWorkloadList := list.(*kueue.WorkloadList); isWorkloadList {
				return errInjected
			}
			return c.List(ctx, list, opts...)
		},
	})
	fieldIndexer := utiltesting.AsIndexer(clientBuilder)
	if err := SetupIndexes(ctx, fieldIndexer); err != nil {
		t.Fatalf("Could not set up indexes: %v", err)
	}
	kClient := clientBuilder.WithObjects(utiltesting.MakeNamespace("ns"), parentJob, childJob).Build()
	reconciler, err := NewReconciler(ctx, kClient, fieldIndexer, &utiltesting.EventRecorder{},
		jobframework.WithIntegrationManager(integrationManager))
	if err != nil {
		t.Fatalf("Could not create the reconciler: %v", err)
	}

	_, err = reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(childJob)})
	if !errors.Is(err, errInjected) {
		t.Fatalf("Reconcile() error = %v, want an error that wraps %v", err, errInjected)
	}
	wantFields := map[string]any{
		"error":       "getting unfinished Workload for ancestor job: injected error",
		"ancestorJob": klog.KObj(parentJob).MarshalLog(),
	}
	if diff := cmp.Diff(wantFields, loggedFields(err)); diff != "" {
		t.Errorf("Unexpected fields of the logged error (-want,+got):\n%s", diff)
	}
}

// loggedFields returns the fields that the Kueue logger writes for err.
func loggedFields(err error) map[string]any {
	core, logs := observer.New(zapcore.ErrorLevel)
	zapr.NewLogger(zap.New(utillogging.NewCustomLogProcessor(core))).Error(err, "Reconciler error")
	return logs.All()[0].ContextMap()
}
