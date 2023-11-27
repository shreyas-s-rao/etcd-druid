package etcd

import (
	"time"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller"
)

const controllerName = "etcd-controller"

// RegisterWithManager registers the Etcd Controller with the given controller manager.
func (r *Reconciler) RegisterWithManager(mgr ctrl.Manager) error {
	builder := ctrl.
		NewControllerManagedBy(mgr).
		Named(controllerName).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: r.config.Workers,
			RateLimiter:             workqueue.NewItemExponentialFailureRateLimiter(10*time.Millisecond, r.config.EtcdStatusSyncPeriod),
		}).
		For(&druidv1alpha1.Etcd{})

	return builder.Complete(r)
}