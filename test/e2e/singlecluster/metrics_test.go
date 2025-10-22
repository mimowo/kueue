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

package e2e

import (
	"time"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/controller/jobs/job"
	utiltesting "sigs.k8s.io/kueue/pkg/util/testing"
	utiltestingapi "sigs.k8s.io/kueue/pkg/util/testing/v1beta1"
	testingjob "sigs.k8s.io/kueue/pkg/util/testingjobs/job"
	"sigs.k8s.io/kueue/test/util"
)

// const (
// 	serviceAccountName           = "kueue-controller-manager"
// 	metricsReaderClusterRoleName = "kueue-metrics-reader"
// )

var _ = ginkgo.Describe("Metrics", func() {
	var (
		ns             *corev1.Namespace
		resourceFlavor *kueue.ResourceFlavor

		// metricsReaderClusterRoleBinding *rbacv1.ClusterRoleBinding

		// curlPod *corev1.Pod
	)

	ginkgo.BeforeEach(func() {
		ns = util.CreateNamespaceWithLog(ctx, k8sClient, "e2e-metrics")

		resourceFlavor = utiltestingapi.MakeResourceFlavor("test-flavor").Obj()
		util.MustCreate(ctx, k8sClient, resourceFlavor)

		// metricsReaderClusterRoleBinding = &rbacv1.ClusterRoleBinding{
		// 	ObjectMeta: metav1.ObjectMeta{Name: "metrics-reader-rolebinding"},
		// 	Subjects: []rbacv1.Subject{
		// 		{
		// 			Kind:      "ServiceAccount",
		// 			Name:      serviceAccountName,
		// 			Namespace: kueueNS,
		// 		},
		// 	},
		// 	RoleRef: rbacv1.RoleRef{
		// 		APIGroup: rbacv1.GroupName,
		// 		Kind:     "ClusterRole",
		// 		Name:     metricsReaderClusterRoleName,
		// 	},
		// }
		// util.MustCreate(ctx, k8sClient, metricsReaderClusterRoleBinding)

		// curlPod = testingjobspod.MakePod("curl-metrics", kueueNS).
		// 	ServiceAccountName(serviceAccountName).
		// 	Image(util.GetAgnHostImage(), util.BehaviorWaitForDeletion).
		// 	TerminationGracePeriod(1).
		// 	Obj()
		// util.MustCreate(ctx, k8sClient, curlPod)

		// ginkgo.By("Waiting for the curl-metrics pod to run.", func() {
		// 	util.WaitForPodRunning(ctx, k8sClient, curlPod)
		// })

		// curlContainerName = curlPod.Spec.Containers[0].Name
	})

	ginkgo.AfterEach(func() {
		gomega.Expect(util.DeleteNamespace(ctx, k8sClient, ns)).To(gomega.Succeed())
		util.ExpectObjectToBeDeleted(ctx, k8sClient, resourceFlavor, true)
		// util.ExpectObjectToBeDeleted(ctx, k8sClient, metricsReaderClusterRoleBinding, true)
		// util.ExpectObjectToBeDeletedWithTimeout(ctx, k8sClient, curlPod, true, util.LongTimeout)
		util.ExpectAllPodsInNamespaceDeleted(ctx, k8sClient, ns)
	})

	ginkgo.When("workload is admitted with admission checks", func() {
		var (
			// admissionCheck  *kueue.AdmissionCheck
			clusterQueue    *kueue.ClusterQueue
			localQueue      *kueue.LocalQueue
			createdJob      *batchv1.Job
			workloadKey     types.NamespacedName
			createdWorkload *kueue.Workload
		)

		ginkgo.BeforeEach(func() {
			// admissionCheck = utiltestingapi.MakeAdmissionCheck("check1").ControllerName("ac-controller").Obj()
			// util.MustCreate(ctx, k8sClient, admissionCheck)

			// util.SetAdmissionCheckActive(ctx, k8sClient, admissionCheck, metav1.ConditionTrue)

			clusterQueue = utiltestingapi.MakeClusterQueue("").
				GeneratedName("test-admission-check-cq-").
				ResourceGroup(
					*utiltestingapi.MakeFlavorQuotas(resourceFlavor.Name).
						Resource(corev1.ResourceCPU, "1").
						Resource(corev1.ResourceMemory, "1Gi").
						Obj(),
				).
				// AdmissionChecks(kueue.AdmissionCheckReference(admissionCheck.Name)).
				Obj()
			util.MustCreate(ctx, k8sClient, clusterQueue)

			localQueue = utiltestingapi.MakeLocalQueue("", ns.Name).
				GeneratedName("test-admission-checked-lq-").
				ClusterQueue(clusterQueue.Name).
				Obj()
			util.MustCreate(ctx, k8sClient, localQueue)

			createdJob = testingjob.MakeJob("admission-checked-job", ns.Name).
				Queue(kueue.LocalQueueName(localQueue.Name)).
				Image(util.GetAgnHostImage(), util.BehaviorWaitForDeletion).
				RequestAndLimit(corev1.ResourceCPU, "1").
				Obj()
			util.MustCreate(ctx, k8sClient, createdJob)

			admissionCheckedJobWLName := job.GetWorkloadNameForJob(createdJob.Name, createdJob.UID)
			workloadKey = types.NamespacedName{
				Name:      admissionCheckedJobWLName,
				Namespace: ns.Name,
			}

			createdWorkload = &kueue.Workload{}

			gomega.Eventually(func(g gomega.Gomega) {
				g.Expect(k8sClient.Get(ctx, workloadKey, createdWorkload)).Should(gomega.Succeed())
				g.Expect(createdWorkload.Status.Conditions).Should(utiltesting.HaveConditionStatusTrue(kueue.WorkloadQuotaReserved))
			}, util.Timeout, util.Interval).Should(gomega.Succeed())
		})

		ginkgo.AfterEach(func() {
			util.ExpectObjectToBeDeleted(ctx, k8sClient, createdJob, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, createdWorkload, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, localQueue, true)
			util.ExpectObjectToBeDeleted(ctx, k8sClient, clusterQueue, true)
			// util.ExpectObjectToBeDeleted(ctx, k8sClient, admissionCheck, true)
		})

		ginkgo.FIt("should ensure the admission check metrics are available", func() {
			// ginkgo.By("setting the check as successful", func() {
			// 	gomega.Eventually(func(g gomega.Gomega) {
			// 		g.Expect(k8sClient.Get(ctx, workloadKey, createdWorkload)).Should(gomega.Succeed())
			// 		// patch := util.BaseSSAWorkload(createdWorkload)
			// 		// workload.SetAdmissionCheckState(&patch.Status.AdmissionChecks, kueue.AdmissionCheckState{
			// 		// 	Name:  kueue.AdmissionCheckReference(admissionCheck.Name),
			// 		// 	State: kueue.CheckStateReady,
			// 		// }, realClock)
			// 		// g.Expect(k8sClient.Status().
			// 		// 	Patch(ctx, patch, client.Apply, client.FieldOwner("test-admission-check-controller"), client.ForceOwnership)).
			// 		// 	Should(gomega.Succeed())
			// 	}, util.Timeout, util.Interval).Should(gomega.Succeed())
			// })
			time.Sleep(time.Second)
			// metrics := [][]string{
			// 	{"kueue_admission_checks_wait_time_seconds", clusterQueue.Name},

			// 	{"kueue_local_queue_admission_checks_wait_time_seconds", ns.Name, localQueue.Name},
			// }

			// ginkgo.By("checking that admission check metrics are available", func() {
			// 	util.ExpectMetricsNotToBeAvailable(ctx, cfg, restClient, curlPod.Name, curlContainerName, metrics)
			// })

			ginkgo.By("deleting the cluster queue", func() {
				util.ExpectObjectToBeDeletedWithTimeout(ctx, k8sClient, createdJob, true, util.VeryLongTimeout)
				util.ExpectObjectToBeDeletedWithTimeout(ctx, k8sClient, createdWorkload, true, util.VeryLongTimeout)
				util.ExpectObjectToBeDeletedWithTimeout(ctx, k8sClient, clusterQueue, true, util.VeryLongTimeout)
			})

			// ginkgo.By("checking that admission check metrics are no longer available", func() {
			// 	util.ExpectMetricsNotToBeAvailable(ctx, cfg, restClient, curlPod.Name, curlContainerName, metrics)
			// })
		})
	})
})
