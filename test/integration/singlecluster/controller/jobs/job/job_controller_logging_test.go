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
	"sync/atomic"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	"go.uber.org/zap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	"sigs.k8s.io/kueue/pkg/controller/core/indexer"
	"sigs.k8s.io/kueue/pkg/controller/jobframework"
	"sigs.k8s.io/kueue/pkg/features"
	testingjob "sigs.k8s.io/kueue/pkg/util/testingjobs/job"
	"sigs.k8s.io/kueue/test/integration/framework"
	"sigs.k8s.io/kueue/test/util/behavioral"
)

var _ = ginkgo.Describe("Job controller logging", ginkgo.Label("job:batch", "area:jobs"), ginkgo.Ordered, func() {
	// failListingWorkloadsOf is the Job whose Workloads the manager's client fails to list.
	var failListingWorkloadsOf atomic.Pointer[types.NamespacedName]

	ginkgo.BeforeAll(func() {
		// The failure is injected through the manager's client, so these tests start their own manager.
		newClient := func(config *rest.Config, options client.Options) (client.Client, error) {
			c, err := client.NewWithWatch(config, options)
			if err != nil {
				return nil, err
			}
			return interceptor.NewClient(c, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if job := failListingWorkloadsOf.Load(); job != nil && listsWorkloadsOwnedBy(*job, list, opts) {
						return errors.New("injected error")
					}
					return c.List(ctx, list, opts...)
				},
			}), nil
		}
		fwk.StartManager(ctx, cfg, managerSetup(
			jobframework.WithManageJobsWithoutQueueName(true),
			jobframework.WithManagedJobsNamespaceSelector(behavioral.NewNamespaceSelectorExcluding("unmanaged-ns")),
		), framework.WithNewClient(newClient))
	})
	ginkgo.AfterAll(func() {
		fwk.StopManager(ctx)
	})

	var ns *corev1.Namespace

	ginkgo.BeforeEach(func() {
		ns = behavioral.CreateNamespaceFromPrefixWithLog(ctx, k8sClient, "job-logging-")
	})

	ginkgo.AfterEach(func() {
		failListingWorkloadsOf.Store(nil)
		gomega.Expect(behavioral.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
	})

	ginkgo.It("Should log the ancestor job as a structured field when getting its Workload fails", func() {
		features.SetFeatureGateDuringTest(ginkgo.GinkgoTB(), features.SkipChildJobSuspension, false)

		ginkgo.By("creating the parent job")
		parentJob := testingjob.MakeJob(parentJobName, ns.Name).PrebuiltWorkloadLabel("missing").Obj()
		behavioral.MustCreate(ctx, k8sClient, parentJob)

		ginkgo.By("failing to list the parent job's Workloads")
		parentKey := client.ObjectKeyFromObject(parentJob)
		failListingWorkloadsOf.Store(&parentKey)

		ginkgo.By("creating the child job")
		childJob := testingjob.MakeJob(childJobName, ns.Name).Suspend(false).Obj()
		gomega.Expect(ctrl.SetControllerReference(parentJob, childJob, k8sClient.Scheme())).To(gomega.Succeed())
		behavioral.MustCreate(ctx, k8sClient, childJob)

		ginkgo.By("checking that the reconciler error has the ancestor job as a structured field")
		ancestorJobField := zap.Any("ancestorJob", klog.KObj(parentJob).MarshalLog())
		gomega.Eventually(func(g gomega.Gomega) {
			entries := fwk.ObservedLogs.FilterMessage("Reconciler error").FilterField(ancestorJobField).All()
			g.Expect(entries).NotTo(gomega.BeEmpty())
			g.Expect(entries[0].ContextMap()).To(gomega.HaveKeyWithValue("error", "getting unfinished Workload for ancestor job: injected error"))
		}, behavioral.Timeout, behavioral.Interval).Should(gomega.Succeed())
	})
})

// listsWorkloadsOwnedBy reports whether a List call lists the Workloads owned by the batch Job.
func listsWorkloadsOwnedBy(job types.NamespacedName, list client.ObjectList, opts []client.ListOption) bool {
	if _, isWorkloadList := list.(*kueue.WorkloadList); !isWorkloadList {
		return false
	}
	listOpts := (&client.ListOptions{}).ApplyOptions(opts)
	if listOpts.Namespace != job.Namespace || listOpts.FieldSelector == nil {
		return false
	}
	owner, found := listOpts.FieldSelector.RequiresExactMatch(indexer.OwnerReferenceIndexKey(batchv1.SchemeGroupVersion.WithKind("Job")))
	return found && owner == job.Name
}
