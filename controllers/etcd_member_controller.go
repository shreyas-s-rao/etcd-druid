// Copyright (c) 2022 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package controllers

import (
	"context"
	"fmt"
	"time"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	"github.com/gardener/etcd-druid/pkg/common"
	componentsts "github.com/gardener/etcd-druid/pkg/component/etcd/statefulset"
	druidpredicates "github.com/gardener/etcd-druid/pkg/predicate"
	"github.com/gardener/etcd-druid/pkg/utils"

	extensionspredicate "github.com/gardener/gardener/extensions/pkg/predicate"
	"github.com/gardener/gardener/pkg/chartrenderer"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/controllerutils"
	utilerrors "github.com/gardener/gardener/pkg/utils/errors"
	"github.com/gardener/gardener/pkg/utils/flow"
	"github.com/gardener/gardener/pkg/utils/imagevector"
	kutil "github.com/gardener/gardener/pkg/utils/kubernetes"
	"github.com/go-logr/logr"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

const (
	operationArgTLSEnabled        = "tlsEnabled"
	operationArgBackupRestorePort = "backupRestorePort"
)

// TODO: check if all the fields are required
// EtcdMemberReconciler reconciles the EtcdMember object.
type EtcdMemberReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	recorder     record.EventRecorder
	chartApplier kubernetes.ChartApplier
	config       *rest.Config
	imageVector  imagevector.ImageVector
	logger       logr.Logger
}

// NewEtcdMemberReconciler creates a new EtcdMemberReconciler.
func NewEtcdMemberReconciler(mgr manager.Manager) (*EtcdMemberReconciler, error) {
	return (&EtcdMemberReconciler{
		Client:   mgr.GetClient(),
		Scheme:   mgr.GetScheme(),
		recorder: mgr.GetEventRecorderFor("etcd-member-controller"),
		config:   mgr.GetConfig(),
		logger:   log.Log.WithName("etcd-member-controller"),
	}).InitializeControllerWithChartApplier()
}

// InitializeControllerWithChartApplier will use EtcdCopyBackupsTaskReconciler rest config to initialize a chart applier.
func (r *EtcdMemberReconciler) InitializeControllerWithChartApplier() (*EtcdMemberReconciler, error) {
	renderer, err := chartrenderer.NewForConfig(r.config)
	if err != nil {
		return nil, err
	}
	applier, err := kubernetes.NewApplierForConfig(r.config)
	if err != nil {
		return nil, err
	}
	r.chartApplier = kubernetes.NewChartApplier(renderer, applier)
	return r, nil
}

// TODO: is this required?
// NewEtcdMemberReconcilerWithImageVector creates a new EtcdMemberReconciler and initializes its image vector.
func NewEtcdMemberReconcilerWithImageVector(mgr manager.Manager) (*EtcdMemberReconciler, error) {
	r, err := NewEtcdMemberReconciler(mgr)
	if err != nil {
		return nil, err
	}
	return r.InitializeControllerWithImageVector()
}

// TODO: is this required?
// InitializeControllerWithImageVector will use EtcdCopyBackupsTaskReconciler client to initialize an image vector.
func (r *EtcdMemberReconciler) InitializeControllerWithImageVector() (*EtcdMemberReconciler, error) {
	imageVector, err := imagevector.ReadGlobalImageVectorWithEnvOverride(getImageYAMLPath())
	if err != nil {
		return nil, err
	}
	r.imageVector = imageVector
	return r, nil
}

// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;list;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;delete

// Reconcile reconciles the EtcdCopyBackupsTask.
func (r *EtcdMemberReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.logger.Info("Etcd member controller reconciliation started")
	etcdMember := &druidv1alpha1.EtcdMember{}
	if err := r.Get(ctx, req.NamespacedName, etcdMember); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !etcdMember.DeletionTimestamp.IsZero() {
		return r.delete(ctx, etcdMember)
	}
	return r.reconcile(ctx, etcdMember)
}

func (r *EtcdMemberReconciler) reconcile(ctx context.Context, etcdMember *druidv1alpha1.EtcdMember) (result ctrl.Result, err error) {
	logger := r.logger.WithValues("etcd-member", kutil.ObjectName(etcdMember), "operation", "reconcile")

	// Ensure finalizer
	if !controllerutil.ContainsFinalizer(etcdMember, FinalizerName) {
		logger.V(1).Info("Adding finalizer")
		if err = controllerutils.PatchAddFinalizers(ctx, r.Client, etcdMember, FinalizerName); err != nil {
			return ctrl.Result{}, fmt.Errorf("could not add finalizer: %w", err)
		}
	}

	if err = r.removeConfirmationAnnotation(ctx, etcdMember); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{
			Requeue: true,
		}, err
	}

	if etcdMember.Spec.Operation != nil {
		if etcdMember.Status.LastOperation == nil {
			if err = r.performOperation(ctx, etcdMember); err != nil {
				return ctrl.Result{Requeue: true}, fmt.Errorf("failed to perform operation type %s with id %s: %v", etcdMember.Spec.Operation.Type, etcdMember.Spec.Operation.ID, err)
			}
		} else {
			if etcdMember.Spec.Operation.ID == etcdMember.Status.LastOperation.ID {
				if etcdMember.Spec.Operation.Type != etcdMember.Status.LastOperation.Type {
					return ctrl.Result{}, fmt.Errorf("cannot re-use ID %s from previous operation %s, for new operation %s", etcdMember.Status.LastOperation.ID, etcdMember.Status.LastOperation.Type, etcdMember.Spec.Operation.Type)
				}
				switch etcdMember.Status.LastOperation.State {
				case druidv1alpha1.LastOperationStateNew, druidv1alpha1.LastOperationStatePending, druidv1alpha1.LastOperationStateError, druidv1alpha1.LastOperationStateProcessing:
					if err = r.performOperation(ctx, etcdMember); err != nil {
						return ctrl.Result{Requeue: true}, fmt.Errorf("failed to perform operation %s with id %s: %v", etcdMember.Spec.Operation.Type, etcdMember.Spec.Operation.ID, err)
					}
				default:
					// do not requeue, since the operation has either succeeded, failed, or been aborted
					return ctrl.Result{}, fmt.Errorf("will not perform operation %s with id %s, as it is in state %s", etcdMember.Status.LastOperation.Type, etcdMember.Status.LastOperation.ID, etcdMember.Status.LastOperation.State)
				}
			} else {
				if err = r.clearLastOperationStatus(ctx, etcdMember); err != nil {
					return ctrl.Result{Requeue: true}, fmt.Errorf("failed to clear lastOperation from status: %v", err)
				}
				if err = r.performOperation(ctx, etcdMember); err != nil {
					return ctrl.Result{Requeue: true}, fmt.Errorf("failed to perform operation type %s with id %s: %v", etcdMember.Spec.Operation.Type, etcdMember.Spec.Operation.ID, err)
				}
			}
		}
	}

	// etcd controller should not update spec if status.lastOperation is not Succeeded|Failed|Aborted
	// etcd controller to update etcd status.etcdMemberRefs when creating etcdMember objects
	// TODO: custodian controller to constantly check etcdMember status.health and update status.status
	// TODO: backup sidecar to update status.health and status.snapshots fields
	// TODO: use ManagedFields to separate updation responsibilities for each field of statuses and use server-side apply

	// etcd controller: create etcdMember resources along with statefulset creation - add etcdMember component
	// etcd controller: check storage-related changes and confirmation annotation to update etcdMember.spec.operation
	// TODO: etcd controller: create etcd.status.lastOperation with etcdOperation info (set state to New)
	// TODO: custodian controller: update etcd.status.lastOperation except `type` field
	// etcd controller: add ownerRef when creating etcdmember resources

	// TODO: etcd controller and etcd member controller should use Flow package effectively to continue operations if left off in the middle due to druid restart

	// requeue if last operation is in `Error` or `Pending` state
	if etcdMember.Status.LastOperation != nil && (etcdMember.Status.LastOperation.State == druidv1alpha1.LastOperationStateError || etcdMember.Status.LastOperation.State == druidv1alpha1.LastOperationStatePending) {
		return ctrl.Result{
			Requeue: true,
		}, nil
	}

	return ctrl.Result{}, nil
}

func (r *EtcdMemberReconciler) removeConfirmationAnnotation(ctx context.Context, etcdMember *druidv1alpha1.EtcdMember) error {
	if _, ok := etcdMember.Annotations[common.PVCDeletionConfirmationAnnotation]; ok {
		r.logger.Info("Removing PVC deletion confirmation annotation")
		withConfirmationAnnotation := etcdMember.DeepCopy()
		delete(etcdMember.Annotations, common.PVCDeletionConfirmationAnnotation)
		return r.Patch(ctx, etcdMember, client.MergeFrom(withConfirmationAnnotation))
	}
	return nil
}

func (r *EtcdMemberReconciler) delete(ctx context.Context, etcd *druidv1alpha1.EtcdMember) (result ctrl.Result, err error) {
	logger := r.logger.WithValues("etcd-member", kutil.Key(etcd.Namespace, etcd.Name).String(), "operation", "delete")
	logger.Info("Starting operation")
	if sets.NewString(etcd.Finalizers...).Has(FinalizerName) {
		logger.Info("Removing finalizer")
		if err := controllerutils.PatchRemoveFinalizers(ctx, r.Client, etcd, FinalizerName); client.IgnoreNotFound(err) != nil {
			return ctrl.Result{
				Requeue: true,
			}, err
		}
	}
	logger.Info("Deleted etcd-member successfully.")
	return ctrl.Result{}, nil
}

func buildEtcdMemberPredicate() predicate.Predicate {
	return predicate.Or(
		druidpredicates.HasConfirmationAnnotation(),
		extensionspredicate.IsDeleting(),
	)
}

// SetupWithManager sets up with the given manager a new controller with r as the ctrl.Reconciler.
func (r *EtcdMemberReconciler) SetupWithManager(mgr ctrl.Manager, workers int) error {
	return ctrl.NewControllerManagedBy(mgr).
		WithEventFilter(buildEtcdMemberPredicate()).
		For(&druidv1alpha1.EtcdMember{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: workers}).
		Complete(r)
}

func (r *EtcdMemberReconciler) updateLastOperationState(ctx context.Context, etcdMember *druidv1alpha1.EtcdMember, state druidv1alpha1.LastOperationState, progress *int32) error {
	return controllerutils.TryUpdateStatus(ctx, retry.DefaultBackoff, r.Client, etcdMember, func() error {
		if etcdMember.Status.LastOperation == nil {
			return fmt.Errorf("status.lastOperation missing")
		}
		etcdMember.Status.LastOperation.State = state
		if progress != nil {
			etcdMember.Status.LastOperation.Progress = *progress
		}
		return nil
	})
}

func (r *EtcdMemberReconciler) clearLastOperationStatus(ctx context.Context, etcdMember *druidv1alpha1.EtcdMember) error {
	return controllerutils.TryUpdateStatus(ctx, retry.DefaultBackoff, r.Client, etcdMember, func() error {
		etcdMember.Status.LastOperation = nil
		return nil
	})
}

func (r *EtcdMemberReconciler) performOperation(ctx context.Context, etcdMember *druidv1alpha1.EtcdMember) error {
	var (
		opState druidv1alpha1.LastOperationState
		opErr   error
	)

	if err := controllerutils.TryUpdateStatus(ctx, retry.DefaultBackoff, r.Client, etcdMember, func() error {
		operation := druidv1alpha1.EtcdMemberLastOperation{
			ID:             etcdMember.Spec.Operation.ID,
			Description:    string(etcdMember.Spec.Operation.Type),
			LastUpdateTime: metav1.Now(),
			Type:           etcdMember.Spec.Operation.Type,
			Progress:       0,
			State:          druidv1alpha1.LastOperationStateNew,
			Reason:         string(druidv1alpha1.LastOperationStateNew),
		}
		etcdMember.Status.LastOperation = &operation
		return nil
	}); err != nil {
		return err
	}

	snapshotType := druidv1alpha1.SnapshotTypeFull
	if etcdMember.Spec.Operation.Type == druidv1alpha1.EtcdMemberOperationTypeTriggerIncrSnapshot {
		snapshotType = druidv1alpha1.SnapshotTypeIncr
	}

	switch etcdMember.Spec.Operation.Type {
	case druidv1alpha1.EtcdMemberOperationTypeTriggerFullSnapshot, druidv1alpha1.EtcdMemberOperationTypeTriggerIncrSnapshot:
		if isLeader, isLeaderErr := isLeader(ctx, etcdMember); isLeaderErr != nil {
			return fmt.Errorf("error in determining leadership of etcd member %s/%s: %v", etcdMember.Namespace, etcdMember.Name, isLeaderErr)
		} else {
			if !isLeader {
				return fmt.Errorf("requested operation type %s with id %s cannot be performed on etcd member %s/%s as it is not the leader", etcdMember.Spec.Operation.Type, etcdMember.Spec.Operation.ID, etcdMember.Namespace, etcdMember.Name)
			}
		}
		opState, opErr = r.performOperationTriggerSnapshot(ctx, etcdMember, snapshotType)
	case druidv1alpha1.EtcdMemberOperationTypeDeletePod:
		opState, opErr = r.performOperationDeletePod(ctx, etcdMember)
	case druidv1alpha1.EtcdMemberOperationTypeDeleteVolume:
		opState, opErr = r.performOperationDeleteVolume(ctx, etcdMember)
	default:
		opState = druidv1alpha1.LastOperationStateNew
		opErr = fmt.Errorf("invalid operation type %s", etcdMember.Spec.Operation.Type)
	}

	if err := controllerutils.TryUpdateStatus(ctx, retry.DefaultBackoff, r.Client, etcdMember, func() error {
		etcdMember.Status.LastOperation.LastUpdateTime = metav1.Now()

		if opErr == nil {
			etcdMember.Status.LastOperation.Progress = 100
			etcdMember.Status.LastOperation.State = druidv1alpha1.LastOperationStateSucceeded
			etcdMember.Status.LastOperation.Reason = string(druidv1alpha1.LastOperationStateSucceeded)
		} else {
			etcdMember.Status.LastOperation.State = opState
			etcdMember.Status.LastOperation.Reason = string(opState)
		}
		return nil
	}); err != nil {
		return err
	}

	return opErr
}

func isLeader(ctx context.Context, etcdMember *druidv1alpha1.EtcdMember) (bool, error) {
	if etcdMember.Status.Role == nil {
		return false, fmt.Errorf("missing membership role info in etcd member status")
	}
	roleLeader := druidv1alpha1.EtcdMemberRoleLeader
	return etcdMember.Status.Role == &roleLeader, nil
}

func GetLeadingEtcdMemberRef(ctx context.Context, etcdMemberList *druidv1alpha1.EtcdMemberList) (*druidv1alpha1.NamespacedObjectReference, error) {
	for _, etcdMember := range etcdMemberList.Items {
		if isLeader, err := isLeader(ctx, &etcdMember); err != nil {
			return nil, err
		} else {
			if isLeader {
				return &druidv1alpha1.NamespacedObjectReference{
					Name:      etcdMember.Name,
					Namespace: etcdMember.Namespace,
				}, nil
			}
		}
	}
	return nil, fmt.Errorf("no leader found in the list of provided etcd members")
}

func parseSnapshotOperationArgs(logger logr.Logger, args map[string]string) (bool, int32, error) {
	var (
		err               error
		tlsEnabled        = false
		backupRestorePort = componentsts.DefaultBackupPort
	)

	if val, ok := args[operationArgTLSEnabled]; ok && val == "true" {
		tlsEnabled = true
	} else {
		logger.Info(fmt.Sprintf("Etcd member operation argument \"%s\" not provided. Defaulting to %t", operationArgTLSEnabled, false))
	}

	if val, ok := args[operationArgBackupRestorePort]; ok {
		logger.Info(fmt.Sprintf("Parsing etcd member operation argument \"%s\" value \"%s\"", operationArgBackupRestorePort, val))
		backupRestorePort, err = utils.ParseInt32(val)
		if err != nil {
			return tlsEnabled, backupRestorePort, err
		}
	} else {
		logger.Info(fmt.Sprintf("Etcd member operation argument \"%s\" not provided. Defaulting to %d", operationArgBackupRestorePort, componentsts.DefaultBackupPort))
	}

	return tlsEnabled, backupRestorePort, nil
}

func (r *EtcdMemberReconciler) doTriggerSnapshot(ctx context.Context, etcdMember *druidv1alpha1.EtcdMember, snapshotType druidv1alpha1.SnapshotType) error {
	tlsEnabled, backupRestorePort, err := parseSnapshotOperationArgs(r.logger, etcdMember.Spec.Operation.Args)
	if err != nil {
		return err
	}

	return utils.TriggerSnapshot(r.config, etcdMember.Name, etcdMember.Namespace, tlsEnabled, backupRestorePort, snapshotType)
}

func (r *EtcdMemberReconciler) doStoreLatestSnapshotTimestamp(ctx context.Context, etcdMember *druidv1alpha1.EtcdMember, snapshotTimestamp *time.Time) error {
	tlsEnabled, backupRestorePort, err := parseSnapshotOperationArgs(r.logger, etcdMember.Spec.Operation.Args)
	if err != nil {
		return err
	}

	timestamp, err := utils.GetLatestSnapshotTimestamp(r.config, etcdMember.Name, etcdMember.Namespace, tlsEnabled, backupRestorePort)
	if err != nil {
		return err
	}

	snapshotTimestamp = timestamp
	return nil
}

func (r *EtcdMemberReconciler) performOperationTriggerSnapshot(ctx context.Context, etcdMember *druidv1alpha1.EtcdMember, snapshotType druidv1alpha1.SnapshotType) (druidv1alpha1.LastOperationState, error) {
	var (
		tasksWithErrors []string
	)

	if etcdMember.Status.LastOperation != nil && etcdMember.Status.LastOperation.TaskID != nil {
		tasksWithErrors = append(tasksWithErrors, *etcdMember.Status.LastOperation.TaskID)
	}
	errorContext := utilerrors.NewErrorContext("Shoot cluster preparation for migration", tasksWithErrors)

	var (
		flowName = fmt.Sprintf("(etcdmember: %s/%s) Deploy Flow for operation %s", etcdMember.Namespace, etcdMember.Name, etcdMember.Spec.Operation.Type)
		g        = flow.NewGraph(flowName)

		taskUpdateStateToProcessing = g.Add(flow.Task{
			Name: "Update lastOperation state to Processing",
			Fn: func(ctx context.Context) error {
				return r.updateLastOperationState(ctx, etcdMember, druidv1alpha1.LastOperationStateProcessing, nil)
			},
			Dependencies: nil,
		})

		oldSnapshotTimestamp          time.Time
		taskStoreOldSnapshotTimestamp = g.Add(flow.Task{
			Name: "Store previous snapshot timestamp",
			Fn: func(ctx context.Context) error {
				return r.doStoreLatestSnapshotTimestamp(ctx, etcdMember, &oldSnapshotTimestamp)
			},
			Dependencies: flow.NewTaskIDs(taskUpdateStateToProcessing),
		})

		taskTriggerSnapshot = g.Add(flow.Task{
			Name:         fmt.Sprintf("Trigger %s snapshot", snapshotType),
			Fn:           func(ctx context.Context) error { return r.doTriggerSnapshot(ctx, etcdMember, snapshotType) },
			Dependencies: flow.NewTaskIDs(taskStoreOldSnapshotTimestamp),
		})

		newSnapshotTimestamp          time.Time
		taskStoreNewSnapshotTimestamp = g.Add(flow.Task{
			Name: "Store new snapshot timestamp",
			Fn: func(ctx context.Context) error {
				return r.doStoreLatestSnapshotTimestamp(ctx, etcdMember, &newSnapshotTimestamp)
			},
			Dependencies: flow.NewTaskIDs(taskTriggerSnapshot),
		})

		_ = g.Add(flow.Task{
			Name: "Compare old and new snapshots",
			Fn: func(ctx context.Context) error {
				if newSnapshotTimestamp.After(oldSnapshotTimestamp) {
					return nil
				} else {
					return fmt.Errorf("failed to trigger new snapshot, since new snapshot is not newer than old snapshot")
				}
			},
			Dependencies: flow.NewTaskIDs(taskStoreNewSnapshotTimestamp),
		})

		f = g.Compile()
	)

	// TODO: vendor latest gardener code to use `Log` instead of `Logger`
	if err := f.Run(ctx, flow.Opts{
		Logger: logrus.New(),
		// TODO: include ProgressReporter for better progress reporting in lastOperation
		// ProgressReporter: r.newProgressReporter(o.ReportShootProgress),
		ErrorContext: errorContext,
	}); err != nil {
		return druidv1alpha1.LastOperationStateError, err
	}

	return druidv1alpha1.LastOperationStateSucceeded, nil
}

func (r *EtcdMemberReconciler) getEmptyPod(ctx context.Context, name, namespace string) *v1.Pod {
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func (r *EtcdMemberReconciler) getEmptyPVC(ctx context.Context, name, namespace string) *v1.PersistentVolumeClaim {
	return &v1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func (r *EtcdMemberReconciler) performOperationDeletePod(ctx context.Context, etcdMember *druidv1alpha1.EtcdMember) (druidv1alpha1.LastOperationState, error) {
	pod := r.getEmptyPod(ctx, etcdMember.Name, etcdMember.Namespace)
	if err := r.Client.Delete(ctx, pod); err != nil {
		return druidv1alpha1.LastOperationStateError, err
	}
	return druidv1alpha1.LastOperationStateProcessing, nil
}

func (r *EtcdMemberReconciler) performOperationDeleteVolume(ctx context.Context, etcdMember *druidv1alpha1.EtcdMember) (druidv1alpha1.LastOperationState, error) {
	if etcdMember.Spec.PVCRef == nil || etcdMember.Spec.PVCRef.Name == "" || etcdMember.Spec.PVCRef.Namespace == "" {
		return druidv1alpha1.LastOperationStateFailed, fmt.Errorf("failed to perform operation %s with id %s: missing details in .spec.pvcRef section", etcdMember.Spec.Operation.Type, etcdMember.Spec.Operation.ID)
	}

	pvc := r.getEmptyPVC(ctx, etcdMember.Spec.PVCRef.Name, etcdMember.Spec.PVCRef.Namespace)
	if err := r.Client.Delete(ctx, pvc); err != nil {
		return druidv1alpha1.LastOperationStateError, err
	}

	if opState, err := r.performOperationDeletePod(ctx, etcdMember); err != nil {
		return opState, err
	}

	return druidv1alpha1.LastOperationStateProcessing, nil
}
