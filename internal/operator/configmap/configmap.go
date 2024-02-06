package configmap

import (
	"encoding/json"
	"fmt"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	"github.com/gardener/etcd-druid/internal/common"
	druiderr "github.com/gardener/etcd-druid/internal/errors"
	"github.com/gardener/etcd-druid/internal/operator/component"
	"github.com/gardener/etcd-druid/internal/utils"
	"github.com/gardener/gardener/pkg/controllerutils"
	gardenerutils "github.com/gardener/gardener/pkg/utils"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	ErrGetConfigMap    druidv1alpha1.ErrorCode = "ERR_GET_CONFIGMAP"
	ErrDeleteConfigMap druidv1alpha1.ErrorCode = "ERR_DELETE_CONFIGMAP"
	ErrSyncConfigMap   druidv1alpha1.ErrorCode = "ERR_SYNC_CONFIGMAP"
)

const etcdConfigKey = "etcd.conf.yaml"

type _resource struct {
	client client.Client
}

func (r _resource) GetExistingResourceNames(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd) ([]string, error) {
	resourceNames := make([]string, 0, 1)
	objKey := getObjectKey(etcd)
	cm := &corev1.ConfigMap{}
	if err := r.client.Get(ctx, objKey, cm); err != nil {
		if errors.IsNotFound(err) {
			return resourceNames, nil
		}
		return nil, druiderr.WrapError(err,
			ErrGetConfigMap,
			"GetExistingResourceNames",
			fmt.Sprintf("Error getting ConfigMap: %v for etcd: %v", objKey, etcd.GetNamespaceName()))
	}
	if metav1.IsControlledBy(cm, etcd) {
		resourceNames = append(resourceNames, cm.Name)
	}
	return resourceNames, nil
}

func (r _resource) Sync(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd) error {
	cm := emptyConfigMap(getObjectKey(etcd))
	result, err := controllerutils.GetAndCreateOrMergePatch(ctx, r.client, cm, func() error {
		return buildResource(etcd, cm)
	})
	if err != nil {
		return druiderr.WrapError(err,
			ErrSyncConfigMap,
			"Sync",
			fmt.Sprintf("Error during create or update of configmap for etcd: %v", etcd.GetNamespaceName()))
	}
	checkSum, err := computeCheckSum(cm)
	if err != nil {
		return druiderr.WrapError(err,
			ErrSyncConfigMap,
			"Sync",
			fmt.Sprintf("Error when computing CheckSum for configmap for etcd: %v", etcd.GetNamespaceName()))
	}
	ctx.Data[common.ConfigMapCheckSumKey] = checkSum
	ctx.Logger.Info("synced", "component", "configmap", "name", cm.Name, "result", result)
	return nil
}

func (r _resource) TriggerDelete(ctx component.OperatorContext, etcd *druidv1alpha1.Etcd) error {
	objectKey := getObjectKey(etcd)
	ctx.Logger.Info("Triggering delete of ConfigMap", "objectKey", objectKey)
	if err := r.client.Delete(ctx, emptyConfigMap(objectKey)); err != nil {
		if errors.IsNotFound(err) {
			ctx.Logger.Info("No ConfigMap found, Deletion is a No-Op", "objectKey", objectKey)
			return nil
		}
		return druiderr.WrapError(
			err,
			ErrDeleteConfigMap,
			"TriggerDelete",
			"Failed to delete configmap",
		)
	}
	ctx.Logger.Info("deleted", "component", "configmap", "objectKey", objectKey)
	return nil
}

func New(client client.Client) component.Operator {
	return &_resource{
		client: client,
	}
}

func buildResource(etcd *druidv1alpha1.Etcd, cm *corev1.ConfigMap) error {
	cfg := createEtcdConfig(etcd)
	cfgYaml, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	cm.Name = etcd.GetConfigMapName()
	cm.Namespace = etcd.Namespace
	cm.Labels = getLabels(etcd)
	cm.OwnerReferences = []metav1.OwnerReference{etcd.GetAsOwnerReference()}
	cm.Data = map[string]string{etcdConfigKey: string(cfgYaml)}

	return nil
}

func getLabels(etcd *druidv1alpha1.Etcd) map[string]string {
	cmLabels := map[string]string{
		druidv1alpha1.LabelComponentKey: common.ConfigMapComponentName,
		druidv1alpha1.LabelAppNameKey:   etcd.GetConfigMapName(),
	}
	return utils.MergeMaps[string, string](etcd.GetDefaultLabels(), cmLabels)
}

func getObjectKey(etcd *druidv1alpha1.Etcd) client.ObjectKey {
	return client.ObjectKey{
		Name:      etcd.GetConfigMapName(),
		Namespace: etcd.Namespace,
	}
}

func emptyConfigMap(objectKey client.ObjectKey) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      objectKey.Name,
			Namespace: objectKey.Namespace,
		},
	}
}

func computeCheckSum(cm *corev1.ConfigMap) (string, error) {
	jsonData, err := json.Marshal(cm.Data)
	if err != nil {
		return "", err
	}
	return gardenerutils.ComputeSHA256Hex(jsonData), nil
}
