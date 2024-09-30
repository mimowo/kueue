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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/kueue/pkg/cache"
	"sigs.k8s.io/kueue/pkg/queue"

	configapi "sigs.k8s.io/kueue/apis/config/v1beta1"
)

func SetupControllers(mgr ctrl.Manager, queues *queue.Manager, cache *cache.Cache, cfg *configapi.Configuration) (string, error) {
	recorder := mgr.GetEventRecorderFor(TASResourceFlavorControllerName)
	rfRec := newRfReconciler(mgr.GetClient(), queues, cache, recorder)
	if err := rfRec.setupWithManager(mgr, cache, cfg); err != nil {
		return "resource_flavor", err
	}
	return "", nil
}
