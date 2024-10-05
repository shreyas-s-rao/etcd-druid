// SPDX-FileCopyrightText: 2024 SAP SE or an SAP affiliate company and Gardener contributors
//
// SPDX-License-Identifier: Apache-2.0

package statefulset

import (
	"fmt"
	"slices"
	"strconv"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	"github.com/gardener/etcd-druid/internal/common"
	"github.com/gardener/etcd-druid/internal/component"
	druiderr "github.com/gardener/etcd-druid/internal/errors"
	"github.com/gardener/etcd-druid/internal/features"
	"github.com/gardener/etcd-druid/internal/utils"

	"github.com/gardener/gardener/pkg/controllerutils"
	"github.com/gardener/gardener/pkg/utils/imagevector"
	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/component-base/featuregate"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// ErrGetStatefulSet indicates an error in getting the statefulset resource.
	ErrGetStatefulSet druidv1alpha1.ErrorCode = "ERR_GET_STATEFULSET"
	// ErrPreSyncStatefulSet indicates an error in pre-sync operations for the statefulset resource.
	ErrPreSyncStatefulSet druidv1alpha1.ErrorCode = "ERR_PRESYNC_STATEFULSET"
	// ErrSyncStatefulSet indicates an error in syncing the statefulset resource.
	ErrSyncStatefulSet druidv1alpha1.ErrorCode = "ERR_SYNC_STATEFULSET"
	// ErrDeleteStatefulSet indicates an error in deleting the statefulset resource.
	ErrDeleteStatefulSet druidv1alpha1.ErrorCode = "ERR_DELETE_STATEFULSET"
)

type _resource struct {
	client         client.Client
	imageVector    imagevector.ImageVector
	useEtcdWrapper bool
	logger         logr.Logger
}

// New returns a new statefulset component operator.
func New(client client.Client, imageVector imagevector.ImageVector, featureGates map[featuregate.Feature]bool) component.Operator {
	return &_resource{
		client:         client,
		imageVector:    imageVector,
		useEtcdWrapper: featureGates[features.UseEtcdWrapper],
	}
}

// GetExistingResourceNames returns the name of the existing statefulset for the given Etcd.
func (r _resource) GetExistingResourceNames(ctx component.OperatorContext, etcdObjMeta metav1.ObjectMeta) ([]string, error) {
	resourceNames := make([]string, 0, 1)
	objectKey := getObjectKey(etcdObjMeta)
	objMeta := &metav1.PartialObjectMetadata{}
	objMeta.SetGroupVersionKind(appsv1.SchemeGroupVersion.WithKind("StatefulSet"))
	if err := r.client.Get(ctx, objectKey, objMeta); err != nil {
		if apierrors.IsNotFound(err) {
			return resourceNames, nil
		}
		return nil, druiderr.WrapError(err,
			ErrGetStatefulSet,
			component.OperationGetExistingResourceNames,
			fmt.Sprintf("Error getting StatefulSet: %v for etcd: %v", objectKey, druidv1alpha1.GetNamespaceName(etcdObjMeta)))
	}
	if metav1.IsControlledBy(objMeta, &etcdObjMeta) {
		resourceNames = append(resourceNames, objMeta.Name)
	}
	return resourceNames, nil
}

// PreSync recreates the statefulset for the given Etcd, if label selector for the existing statefulset
// is different from the label selector required to be applied on it. This is because the statefulset's
// spec.selector field is immutable and cannot be updated on the existing statefulset.
func (r _resource) PreSync(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd) error {
	r.logger = ctx.Logger.WithValues("component", component.StatefulSetKind, "operation", component.OperationPreSync)
	r.logger.Info("Running pre-sync for StatefulSet")

	sts, err := r.getExistingStatefulSet(ctx, etcd.ObjectMeta)
	if err != nil {
		return druiderr.WrapError(err,
			ErrPreSyncStatefulSet,
			component.OperationPreSync,
			fmt.Sprintf("Error getting StatefulSet: %v for etcd: %v", getObjectKey(etcd.ObjectMeta), druidv1alpha1.GetNamespaceName(etcd.ObjectMeta)))
	}
	// if no sts exists, this method is a no-op.
	if sts != nil {
		if err = r.handleStsPodLabelsOnMismatch(ctx, etcd, sts); err != nil {
			return err
		}
		if err = r.handleStsLabelSelectorOnMismatch(ctx, etcd, sts); err != nil {
			return err
		}
	}
	return nil
}

// Sync creates or updates the statefulset for the given Etcd.
func (r _resource) Sync(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd) error {
	r.logger = ctx.Logger.WithValues("component", component.StatefulSetKind, "operation", component.OperationSync)
	objectKey := getObjectKey(etcd.ObjectMeta)
	existingSTS, err := r.getExistingStatefulSet(ctx, etcd.ObjectMeta)
	if err != nil {
		return druiderr.WrapError(err,
			ErrSyncStatefulSet,
			component.OperationSync,
			fmt.Sprintf("Error getting StatefulSet: %v for etcd: %v", objectKey, druidv1alpha1.GetNamespaceName(etcd.ObjectMeta)))
	}
	// There is no StatefulSet present. Create one.
	if existingSTS == nil {
		// check if sts is orphan deleted in the PreSync step and there are orphaned pods that exist.
		recreated, err := r.recreateStsIfOrphanDeleted(ctx, etcd)
		if err != nil {
			return err
		}
		if !recreated {
			return r.createOrPatch(ctx, etcd)
		}
	}
	// StatefulSet exists, check if TLS has been enabled for peer communication, if yes then it is currently a multistep
	// process to ensure that all members are updated and establish peer TLS communication.
	// There is no need to do this check if the etcd.Spec.Replicas have been set to 0 (scale down to 0 case).
	// TODO: Once we support scale down to non-zero replicas then we will need to adjust the replicas for which we check for TLS.
	if etcd.Spec.Replicas > 0 {
		if err = r.handleTLSChanges(ctx, etcd, existingSTS); err != nil {
			return err
		}
	}
	return r.createOrPatch(ctx, etcd)
}

// TriggerDelete triggers the deletion of the statefulset for the given Etcd.
func (r _resource) TriggerDelete(ctx component.OperatorContext, etcdObjMeta metav1.ObjectMeta) error {
	r.logger = ctx.Logger.WithValues("component", component.StatefulSetKind, "operation", component.OperationTriggerDelete)
	objectKey := getObjectKey(etcdObjMeta)
	r.logger.Info("Triggering deletion of StatefulSet")
	if err := r.client.Delete(ctx, emptyStatefulSet(etcdObjMeta)); err != nil {
		if apierrors.IsNotFound(err) {
			ctx.Logger.Info("No StatefulSet found, Deletion is a No-Op", "objectKey", objectKey.Name)
			return nil
		}
		return druiderr.WrapError(err,
			ErrDeleteStatefulSet,
			component.OperationTriggerDelete,
			fmt.Sprintf("Failed to delete StatefulSet: %v for etcd %v", objectKey, druidv1alpha1.GetNamespaceName(etcdObjMeta)))
	}
	r.logger.Info("deletion successful")
	return nil
}

func (r _resource) handleStsPodLabelsOnMismatch(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd, sts *appsv1.StatefulSet) error {
	if err := r.checkAndPatchStsPodLabelsOnMismatch(ctx, etcd, sts); err != nil {
		return druiderr.WrapError(err,
			ErrPreSyncStatefulSet,
			component.OperationPreSync,
			fmt.Sprintf("Error checking and patching StatefulSet pods with new labels for etcd: %v", druidv1alpha1.GetNamespaceName(etcd.ObjectMeta)))
	}

	// check if pods have been updated with new labels.
	podsHaveDesiredLabels, err := r.doStatefulSetPodsHaveDesiredLabels(ctx, etcd, sts)
	if err != nil {
		return druiderr.WrapError(err,
			ErrPreSyncStatefulSet,
			component.OperationPreSync,
			fmt.Sprintf("Error checking if StatefulSet pods are updated for etcd: %v", druidv1alpha1.GetNamespaceName(etcd.ObjectMeta)))
	}
	if !podsHaveDesiredLabels {
		return druiderr.New(druiderr.ErrRequeueAfter,
			component.OperationPreSync,
			fmt.Sprintf("StatefulSet pods are not yet updated with new labels, for StatefulSet: %v for etcd: %v", getObjectKey(sts.ObjectMeta), druidv1alpha1.GetNamespaceName(etcd.ObjectMeta)),
		)
	} else {
		r.logger.Info("StatefulSet pods have all the desired labels", "objectKey", getObjectKey(etcd.ObjectMeta))
	}
	return nil
}

func (r _resource) handleStsLabelSelectorOnMismatch(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd, sts *appsv1.StatefulSet) error {
	if labels.Equals(sts.Spec.Selector.MatchLabels, druidv1alpha1.GetDefaultLabels(etcd.ObjectMeta)) {
		return nil
	}
	r.logger.Info("Orphan deleting StatefulSet for recreation later, as label selector has changed", "oldSelector.MatchLabels", sts.Spec.Selector.MatchLabels, "newOldSelector.MatchLabels", druidv1alpha1.GetDefaultLabels(etcd.ObjectMeta))
	if err := r.client.Delete(ctx, sts, client.PropagationPolicy(metav1.DeletePropagationOrphan)); err != nil {
		return druiderr.WrapError(err,
			ErrPreSyncStatefulSet,
			component.OperationPreSync,
			fmt.Sprintf("Error orphan deleting StatefulSet: %v for etcd: %v", client.ObjectKeyFromObject(sts), client.ObjectKeyFromObject(sts)))
	}
	// check if sts has been orphan delete. If not then requeue.
	foundSts, err := r.getExistingStatefulSet(ctx, sts.ObjectMeta)
	if err != nil {
		return druiderr.WrapError(err,
			ErrPreSyncStatefulSet,
			component.OperationPreSync,
			fmt.Sprintf("Error checking if StatefulSet has been orphan deleted: %v for etcd: %v", client.ObjectKeyFromObject(sts), client.ObjectKeyFromObject(sts)))
	}
	if foundSts == nil {
		r.logger.Info("StatefulSet has been orphan deleted")
		return nil
	}
	return druiderr.New(
		druiderr.ErrRequeueAfter,
		component.OperationPreSync,
		fmt.Sprintf("StatefulSet has not been orphan deleted: %v for etcd: %v, requeuing reconcile request", client.ObjectKeyFromObject(sts), client.ObjectKeyFromObject(sts)))
}

func (r _resource) recreateStsIfOrphanDeleted(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd) (recreated bool, err error) {
	var etcdClusterSize int
	orphanedPodsObjMeta, err := r.getOrphanedPodsObjMeta(ctx, etcd)
	if err != nil {
		return
	}
	if len(orphanedPodsObjMeta) > 0 {
		var previousReplicas int
		// First try and get the previous etcd cluster size by looking at LabelEtcdClusterSizeKey label on the orphaned pods.
		// In case the label is not set (Only if etcd cluster pods are created from version v0.23.1 onwards, will this label be present)
		// then fall back to the number of orphaned pods. The reason we do that is that it is not guaranteed that
		// all orphaned pods are still around. It is possible that they get evicted due to node crash and since there is no STS it will also not get reconciled.
		etcdClusterSizeStr, ok := orphanedPodsObjMeta[0].Labels[druidv1alpha1.LabelEtcdClusterSizeKey]
		if !ok {
			r.logger.Info("LabelEtcdClusterSizeKey not found on orphaned pod, falling back to number of orphaned pods", "orphanedPodsCount", len(orphanedPodsObjMeta))
			previousReplicas = len(orphanedPodsObjMeta)
		} else {
			etcdClusterSize, err = strconv.Atoi(etcdClusterSizeStr)
			if err != nil {
				r.logger.Error(err, "Error parsing etcd cluster size from orphaned pod, falling back to number of orphaned pods", "etcdClusterSizeStr", etcdClusterSizeStr)
			}
			previousReplicas = etcdClusterSize
		}
		r.logger.Info("Recreating StatefulSet with previous replicas", "previousReplicas", previousReplicas)
		if err = r.createOrPatchWithReplicas(ctx, etcd, int32(previousReplicas)); err != nil {
			err = druiderr.WrapError(err,
				ErrPreSyncStatefulSet,
				component.OperationPreSync,
				fmt.Sprintf("Error creating StatefulSet with previous replicas for orphan pods adoption for etcd: %v", druidv1alpha1.GetNamespaceName(etcd.ObjectMeta)))
			return
		}
		r.logger.Info("Requeuing reconcile request to allow adoption of orphaned pods")
		err = druiderr.New(
			druiderr.ErrRequeueAfter,
			component.OperationPreSync,
			fmt.Sprintf("StatefulSet has not yet been created or is not ready with previous replicas for etcd: %v, requeuing reconcile request", druidv1alpha1.GetNamespaceName(etcd.ObjectMeta)))
		recreated = true
	}
	return
}

func (r _resource) getOrphanedPodsObjMeta(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd) ([]metav1.PartialObjectMetadata, error) {
	objMetaList := &metav1.PartialObjectMetadataList{}
	objMetaList.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Pod"))
	if err := r.client.List(ctx,
		objMetaList,
		client.InNamespace(etcd.Namespace),
		client.MatchingLabels(druidv1alpha1.GetDefaultLabels(etcd.ObjectMeta)),
	); err != nil {
		if apierrors.IsNotFound(err) {
			return []metav1.PartialObjectMetadata{}, nil
		}
		return nil, err
	}
	return objMetaList.Items, nil
}

// getExistingStatefulSet gets the existing statefulset if it exists.
// If it is not found, it simply returns nil. Any other errors are returned as is.
func (r _resource) getExistingStatefulSet(ctx component.OperatorContext, etcdObjMeta metav1.ObjectMeta) (*appsv1.StatefulSet, error) {
	sts := emptyStatefulSet(etcdObjMeta)
	if err := r.client.Get(ctx, getObjectKey(etcdObjMeta), sts); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return sts, nil
}

// createOrPatchWithReplicas ensures that the StatefulSet is updated with all changes from passed in etcd but the replicas set on the StatefulSet
// are taken from the passed in replicas and not from the etcd component.
func (r _resource) createOrPatchWithReplicas(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd, replicas int32) error {
	desiredStatefulSet := emptyStatefulSet(etcd.ObjectMeta)
	mutatingFn := func() error {
		if builder, err := newStsBuilder(r.client, ctx.Logger, etcd, replicas, r.useEtcdWrapper, r.imageVector, desiredStatefulSet); err != nil {
			return err
		} else {
			return builder.Build(ctx)
		}
	}
	opResult, err := controllerutils.GetAndCreateOrStrategicMergePatch(ctx, r.client, desiredStatefulSet, mutatingFn)
	if err != nil {
		return err
	}
	r.logger.Info("triggered create/patch of statefulSet", "operationResult", opResult)
	return nil
}

// createOrPatch updates StatefulSet taking changes from passed in etcd component.
func (r _resource) createOrPatch(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd) error {
	if err := r.createOrPatchWithReplicas(ctx, etcd, etcd.Spec.Replicas); err != nil {
		return druiderr.WrapError(err,
			ErrSyncStatefulSet,
			component.OperationSync,
			fmt.Sprintf("Error creating or patching [StatefulSet: %v, Replicas: %d] for etcd: %v", client.ObjectKeyFromObject(etcd), etcd.Spec.Replicas, client.ObjectKeyFromObject(etcd)))
	}
	return nil
}

func (r _resource) handleTLSChanges(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd, existingSts *appsv1.StatefulSet) error {
	isSTSTLSConfigNotInSync := r.isStatefulSetTLSConfigNotInSync(etcd, existingSts)
	if isSTSTLSConfigNotInSync {
		if err := r.createOrPatchWithReplicas(ctx, etcd, *existingSts.Spec.Replicas); err != nil {
			return druiderr.WrapError(err,
				ErrSyncStatefulSet,
				component.OperationSync,
				fmt.Sprintf("Error creating or patching StatefulSet with TLS changes for StatefulSet: %v, etcd: %v", client.ObjectKeyFromObject(existingSts), client.ObjectKeyFromObject(etcd)))
		}
	}
	peerTLSInSyncForAllMembers, err := utils.IsPeerURLInSyncForAllMembers(ctx, r.client, ctx.Logger, etcd, *existingSts.Spec.Replicas)
	if err != nil {
		return druiderr.WrapError(err,
			ErrSyncStatefulSet,
			component.OperationSync,
			fmt.Sprintf("Error checking if peer TLS is enabled for statefulset: %v, etcd: %v", client.ObjectKeyFromObject(existingSts), client.ObjectKeyFromObject(etcd)))
	}
	if peerTLSInSyncForAllMembers {
		r.logger.Info("Peer URL TLS configuration is reflected on all currently running members")
		return nil
	} else {
		return druiderr.New(
			druiderr.ErrRequeueAfter,
			component.OperationSync,
			fmt.Sprintf("Peer URL TLS not enabled for #%d members for etcd: %v, requeuing reconcile request", *existingSts.Spec.Replicas, client.ObjectKeyFromObject(etcd)))
	}
}

func (r _resource) isStatefulSetTLSConfigNotInSync(etcd *druidv1alpha1.Etcd, sts *appsv1.StatefulSet) bool {
	newEtcdbrTLSVolMounts := getBackupRestoreContainerSecretVolumeMounts(etcd)
	newEtcdWrapperTLSVolMounts := getEtcdContainerSecretVolumeMounts(etcd)
	containerTLSVolMounts := utils.GetStatefulSetContainerTLSVolumeMounts(sts)
	return hasTLSVolumeMountsChanged(containerTLSVolMounts[common.ContainerNameEtcd], newEtcdWrapperTLSVolMounts) ||
		hasTLSVolumeMountsChanged(containerTLSVolMounts[common.ContainerNameEtcdBackupRestore], newEtcdbrTLSVolMounts)
}

func hasTLSVolumeMountsChanged(existingVolMounts, newVolMounts []corev1.VolumeMount) bool {
	if len(existingVolMounts) != len(newVolMounts) {
		return true
	}
	for _, newVolMount := range newVolMounts {
		if !slices.ContainsFunc(existingVolMounts, func(existingVolMount corev1.VolumeMount) bool {
			return existingVolMount.Name == newVolMount.Name && existingVolMount.MountPath == newVolMount.MountPath
		}) {
			return true
		}
	}
	return false
}

func (r _resource) checkAndPatchStsPodLabelsOnMismatch(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd, sts *appsv1.StatefulSet) error {
	desiredPodTemplateLabels := getDesiredPodTemplateLabels(etcd)
	if !utils.ContainsAllDesiredLabels(sts.Spec.Template.Labels, desiredPodTemplateLabels) {
		r.logger.Info("Patching StatefulSet with new pod labels", "desiredPodTemplateLabels", desiredPodTemplateLabels)
		originalSts := sts.DeepCopy()
		sts.Spec.Template.Labels = utils.MergeMaps(sts.Spec.Template.Labels, desiredPodTemplateLabels)
		if err := r.client.Patch(ctx, sts, client.MergeFrom(originalSts)); err != nil {
			return err
		}
	}
	return nil
}

func getDesiredPodTemplateLabels(etcd *druidv1alpha1.Etcd) map[string]string {
	return utils.MergeMaps(etcd.Spec.Labels, getStatefulSetLabels(etcd.Name))
}

func (r _resource) doStatefulSetPodsHaveDesiredLabels(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd, sts *appsv1.StatefulSet) (bool, error) {
	// sts.spec.replicas is more accurate than Etcd.spec.replicas, specifically when
	// Etcd.spec.replicas is updated but not yet reflected in the etcd cluster
	if sts.Spec.Replicas == nil {
		return false, fmt.Errorf("statefulset %s does not have a replicas count defined", sts.Name)
	}
	podNames := druidv1alpha1.GetAllPodNames(etcd.ObjectMeta, *sts.Spec.Replicas)
	desiredLabels := getDesiredPodTemplateLabels(etcd)
	for _, podName := range podNames {
		pod := &corev1.Pod{}
		if err := r.client.Get(ctx, client.ObjectKey{Name: podName, Namespace: etcd.Namespace}, pod); err != nil {
			return false, err
		}
		if !utils.ContainsAllDesiredLabels(pod.Labels, desiredLabels) {
			return false, nil
		}
	}
	return true, nil
}

func emptyStatefulSet(obj metav1.ObjectMeta) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            druidv1alpha1.GetStatefulSetName(obj),
			Namespace:       obj.Namespace,
			OwnerReferences: []metav1.OwnerReference{druidv1alpha1.GetAsOwnerReference(obj)},
		},
	}
}

func getObjectKey(obj metav1.ObjectMeta) client.ObjectKey {
	return client.ObjectKey{
		Name:      obj.Name,
		Namespace: obj.Namespace,
	}
}
