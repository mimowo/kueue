/*
Copyright 2024 The Kubernetes Authors.

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

package tas

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueuealpha "sigs.k8s.io/kueue/apis/kueue/v1alpha1"

	configapi "sigs.k8s.io/kueue/apis/config/v1beta1"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta1"
	"sigs.k8s.io/kueue/pkg/cache"
	kueueconstants "sigs.k8s.io/kueue/pkg/controller/constants"
	"sigs.k8s.io/kueue/pkg/controller/core"
	"sigs.k8s.io/kueue/pkg/queue"
	utiltas "sigs.k8s.io/kueue/pkg/util/tas"
)

const (
	ungateBatchPeriod = time.Second
)

type topologyUngater struct {
	client   client.Client
	recorder record.EventRecorder
}

var _ reconcile.Reconciler = (*topologyUngater)(nil)

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads,verbs=get;list;watch
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads/status,verbs=get

func newTopologyUngater(c client.Client, queues *queue.Manager, cache *cache.Cache, recorder record.EventRecorder) *topologyUngater {
	return &topologyUngater{
		client:   c,
		recorder: recorder,
	}
}

func (r *topologyUngater) setupWithManager(mgr ctrl.Manager, cache *cache.Cache, cfg *configapi.Configuration) error {
	podHandler := podHandler{}
	return ctrl.NewControllerManagedBy(mgr).
		Named("tas-resource-flavor").
		For(&kueue.Workload{}).
		Watches(&corev1.Pod{}, &podHandler).
		WithOptions(controller.Options{NeedLeaderElection: ptr.To(false)}).
		WithEventFilter(r).
		Complete(core.WithLeadingManager(mgr, r, &kueue.ClusterQueue{}, cfg))
}

var _ handler.EventHandler = (*podHandler)(nil)

// nodeHandler handles node update events.
type podHandler struct {
}

func (h *podHandler) Create(_ context.Context, e event.CreateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	pod, isPod := e.Object.(*corev1.Pod)
	if !isPod {
		return
	}
	h.queueReconcileForPod(pod, q)
}

func (h *podHandler) Update(ctx context.Context, e event.UpdateEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	oldPod, isOldPod := e.ObjectOld.(*corev1.Pod)
	newPod, isNewPod := e.ObjectNew.(*corev1.Pod)
	if !isOldPod || !isNewPod {
		return
	}
	h.queueReconcileForPod(oldPod, q)
	h.queueReconcileForPod(newPod, q)
}

func (h *podHandler) Delete(_ context.Context, e event.DeleteEvent, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	pod, isPod := e.Object.(*corev1.Pod)
	if !isPod {
		return
	}
	h.queueReconcileForPod(pod, q)
}

func (h *podHandler) queueReconcileForPod(pod *corev1.Pod, q workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if pod == nil {
		return
	}
	if len(pod.Spec.SchedulingGates) == 0 {
		return
	}
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == kueuealpha.TopologySchedulingGate {
			if wlName, found := pod.Annotations[kueueconstants.WorkloadAnnotation]; found {
				q.AddAfter(reconcile.Request{NamespacedName: types.NamespacedName{
					Name:      string(wlName),
					Namespace: pod.Namespace,
				}}, ungateBatchPeriod)
			}
		}
	}
}

func (h *podHandler) Generic(context.Context, event.GenericEvent, workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

func (r *topologyUngater) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	log := ctrl.LoggerFrom(ctx).WithValues("workload", req.NamespacedName.Name)
	log.V(2).Info("Reconcile Topology Ungater")

	wl := &kueue.Workload{}
	if err := r.client.Get(ctx, req.NamespacedName, wl); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return reconcile.Result{}, err
		}
		log.Info("not found")
	}
	if wl.Status.Admission == nil {
		return reconcile.Result{}, nil
	}
	for _, psa := range wl.Status.Admission.PodSetAssignments {
		if psa.TopologyAssignment != nil {
			r.ungatePodSet(ctx, wl, &psa)
		}
	}
	return reconcile.Result{}, nil
}

func (r *topologyUngater) Create(event event.CreateEvent) bool {
	wl, isWl := event.Object.(*kueue.Workload)
	if isWl {
		return isTASWorkload(wl)
	}
	return false
}

func (r *topologyUngater) Delete(event event.DeleteEvent) bool {
	wl, isWl := event.Object.(*kueue.Workload)
	if isWl {
		return isTASWorkload(wl)
	}
	return false
}

func (r *topologyUngater) Update(event event.UpdateEvent) bool {
	_, isOldWl := event.ObjectOld.(*kueue.Workload)
	newWl, isNewWl := event.ObjectNew.(*kueue.Workload)
	if isOldWl && isNewWl {
		return isTASWorkload(newWl)
	}
	return false
}

func isTASWorkload(wl *kueue.Workload) bool {
	if wl.Status.Admission == nil {
		return false
	}
	for _, psa := range wl.Status.Admission.PodSetAssignments {
		if psa.TopologyAssignment != nil {
			return true
		}
	}
	return false
}

func (r *topologyUngater) Generic(event event.GenericEvent) bool {
	return false
}

func (r *topologyUngater) ungatePodSet(_ context.Context, _ *kueue.Workload, psa *kueue.PodSetAssignment) {
	expectedCountPerDomainId := make(map[utiltas.TopologyDomainId]int32)
	for _, domain := range psa.TopologyAssignment.Domains {
		domainId := utiltas.DomainIdForAssignment(domain.Levels)
		expectedCountPerDomainId[domainId] = int32(domain.Count)
	}
}
