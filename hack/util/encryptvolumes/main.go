package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"k8s.io/client-go/tools/clientcmd/api"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gardener/etcd-druid/api/v1alpha1"
	druidkubernetes "github.com/gardener/etcd-druid/pkg/client/kubernetes"
	"github.com/gardener/etcd-druid/pkg/utils"

	"github.com/gardener/etcd-backup-restore/pkg/miscellaneous"
	"github.com/gardener/etcd-backup-restore/pkg/snapstore"
	brtypes "github.com/gardener/etcd-backup-restore/pkg/types"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	v1beta1constants "github.com/gardener/gardener/pkg/apis/core/v1beta1/constants"
	"github.com/gardener/gardener/pkg/controllerutils"
	gardenerretry "github.com/gardener/gardener/pkg/utils/retry"
	"go.etcd.io/etcd/etcdserver/etcdserverpb"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	KUBECONFIG         = "KUBECONFIG"
	AWS_REGION         = "AWS_REGION"
	debugContainerName = "volume-replacement-task"
	kapiDeploymentName = "kube-apiserver"
	kapiLabelRoleKey   = "role"
)

// getEtcds gets etcds with the given name, across namespaces
func getEtcds(c client.Client, etcdName string) ([]v1alpha1.Etcd, error) {
	etcds := &v1alpha1.EtcdList{}
	if err := c.List(context.Background(), etcds, client.MatchingFields{"metadata.name": etcdName}); err != nil {
		return nil, fmt.Errorf("unable to list etcds: %v", err)
	}
	return etcds.Items, nil
}

func isEtcdScaledDown(etcd *v1alpha1.Etcd) bool {
	return etcd.Spec.Replicas == 0
}

func getEtcdStatefulSetLabels(c client.Client, etcd *v1alpha1.Etcd) (map[string]string, error) {
	sts := &appsv1.StatefulSet{}
	err := c.Get(context.Background(), types.NamespacedName{Namespace: etcd.Namespace, Name: etcd.Name}, sts)
	if err != nil {
		return nil, fmt.Errorf("unable to get statefulset: %v", err)
	}
	return sts.Labels, nil
}

func getPVCsForEtcd(c client.Client, etcd *v1alpha1.Etcd, labels map[string]string) ([]corev1.PersistentVolumeClaim, error) {
	pvcList := &corev1.PersistentVolumeClaimList{}
	if err := c.List(context.Background(), pvcList, client.InNamespace(etcd.Namespace), client.MatchingLabels(labels)); err != nil {
		return nil, fmt.Errorf("unable to list pvcs: %v", err)
	}
	if len(pvcList.Items) == 2 {
		// If there are 2 PVCs, it means that PVC *-0 was created long ago and has different labels than expected.
		pvcName0 := fmt.Sprintf("%s%d", pvcList.Items[0].Name[:len(pvcList.Items[0].Name)-1], 0)
		log.Printf("Found 2 pvcs for etcd %s/%s. Fetching the missing PVC %s.", etcd.Namespace, etcd.Name, pvcName0)
		pvc := corev1.PersistentVolumeClaim{}
		if err := c.Get(context.Background(), types.NamespacedName{Namespace: etcd.Namespace, Name: pvcName0}, &pvc); err != nil {
			return nil, fmt.Errorf("unable to get pvc: %v", err)
		}
		return append([]corev1.PersistentVolumeClaim{pvc}, pvcList.Items...), nil
	}
	return pvcList.Items, nil
}

func scaleEtcd(c client.Client, etcd *v1alpha1.Etcd, replicas int32, stsLabels map[string]string) error {
	log.Printf("Scaling etcd %s/%s from %d to %d replicas", etcd.Namespace, etcd.Name, etcd.Spec.Replicas, replicas)

	result, err := controllerutils.GetAndCreateOrMergePatch(context.Background(), c, etcd, func() error {
		etcd.Spec.Replicas = replicas
		etcd.Annotations[v1beta1constants.GardenerOperation] = v1beta1constants.GardenerOperationReconcile
		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to update etcd: %v", err)
	}
	if result != controllerutil.OperationResultUpdated {
		return fmt.Errorf("etcd not updated")
	}

	return gardenerretry.UntilTimeout(context.Background(), 2*time.Second, 5*time.Minute, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, types.NamespacedName{Namespace: etcd.Namespace, Name: etcd.Name}, etcd); err != nil {
			return gardenerretry.SevereError(fmt.Errorf("failed to get etcd: %v", err))
		}
		if etcd.Status.Replicas != replicas || etcd.Status.ReadyReplicas != replicas {
			return gardenerretry.MinorError(fmt.Errorf("etcd not scaled yet"))
		}
		pods := &corev1.PodList{}
		if err := c.List(ctx, pods, client.InNamespace(etcd.Namespace), client.MatchingLabels(stsLabels)); err != nil {
			if apierrors.IsNotFound(err) {
				if replicas == 0 {
					return gardenerretry.Ok()
				}
				return gardenerretry.MinorError(err)
			}
			return gardenerretry.SevereError(err)
		}
		if len(pods.Items) != int(replicas) {
			return gardenerretry.MinorError(fmt.Errorf("etcd not scaled yet"))
		}

		readyPods := 0
		for _, pod := range pods.Items {
			if pod.Status.Phase == corev1.PodRunning {
				readyPods++
			}
		}
		if readyPods != int(replicas) {
			return gardenerretry.MinorError(fmt.Errorf("etcd not scaled yet"))
		}

		return gardenerretry.Ok()
	})
}

func scaleUpEtcd(c client.Client, etcd *v1alpha1.Etcd, replicas int32, stsLabels map[string]string) error {
	return scaleEtcd(c, etcd, replicas, stsLabels)
}

func scaleDownEtcd(c client.Client, etcd *v1alpha1.Etcd, stsLabels map[string]string) error {
	return scaleEtcd(c, etcd, 0, stsLabels)
}

func isSingleNodeEtcd(etcd *v1alpha1.Etcd, volumeCount int) bool {
	return etcd.Spec.Replicas == 1 || volumeCount == 1
}

func isMultiNodeEtcd(etcd *v1alpha1.Etcd, volumeCount int) bool {
	return etcd.Spec.Replicas == 3 || volumeCount == 3
}

func getEtcdPods(c client.Client, etcd *v1alpha1.Etcd, stsLabels map[string]string) ([]corev1.Pod, error) {
	podList := &corev1.PodList{}
	if err := c.List(context.Background(), podList, client.InNamespace(etcd.Namespace), client.MatchingLabels(stsLabels)); err != nil {
		return nil, fmt.Errorf("unable to list pods: %v", err)
	}
	return podList.Items, nil

	// TODO: remove
	//var pods []*corev1.Pod
	//for _, member := range etcd.Status.Members {
	//	pod := &corev1.Pod{}
	//	err := c.Get(context.Background(), types.NamespacedName{Namespace: etcd.Namespace, Name: member.Name}, pod)
	//	if err != nil {
	//		return nil, fmt.Errorf("unable to get pod %s: %v", member.Name, err)
	//	}
	//	pods = append(pods, pod)
	//}
	//return pods, nil
}

func ensureAllMembersReady(c client.Client, etcd *v1alpha1.Etcd, expectedPodCount int) error {
	// TODO: remove
	//var allMembersReadyCondition *v1alpha1.Condition
	//for _, cond := range etcd.Status.Conditions {
	//	if cond.Type == v1alpha1.ConditionTypeAllMembersReady {
	//		allMembersReadyCondition = &cond
	//		break
	//	}
	//}
	//if allMembersReadyCondition != nil {
	//	if allMembersReadyCondition.Status != v1alpha1.ConditionTrue {
	//		return fmt.Errorf("condition %s is %s instead of %s", v1alpha1.ConditionTypeAllMembersReady, allMembersReadyCondition.Status, v1alpha1.ConditionTrue)
	//	}
	//} else {
	//	return fmt.Errorf("condition %s is missing", v1alpha1.ConditionTypeAllMembersReady)
	//}

	stsLabels, err := getEtcdStatefulSetLabels(c, etcd)
	if err != nil {
		return fmt.Errorf("unable to get statefulset labels for etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
	}

	pods, err := getEtcdPods(c, etcd, stsLabels)
	if err != nil {
		return fmt.Errorf("unable to get pods: %v", err)
	}
	if len(pods) != expectedPodCount {
		return fmt.Errorf("expected %d pods, got %d", expectedPodCount, len(pods))
	}

	var errs []error
	for _, pod := range pods {
		var podReady, containersReady bool
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady {
				podReady = cond.Status == corev1.ConditionTrue
			}
			if cond.Type == corev1.ContainersReady {
				containersReady = cond.Status == corev1.ConditionTrue
			}
		}
		if !podReady || !containersReady {
			errs = append(errs, fmt.Errorf("pod %s is not ready yet", pod.Name))
		}
		if pod.Status.Phase != corev1.PodRunning {
			errs = append(errs, fmt.Errorf("pod %s is not running yet", pod.Name))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

func executeContainerCommand(config *rest.Config, clientSet kubernetes.Clientset, podNamespace, podName, containerName, command string) (string, error) {
	req := clientSet.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(podNamespace).
		SubResource("exec").
		Param("container", containerName).
		VersionedParams(&corev1.PodExecOptions{
			Command: []string{"/bin/bash", "-c", command},
			Stdin:   false,
			Stdout:  true,
			Stderr:  true,
			TTY:     false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(config, "POST", req.URL())
	if err != nil {
		return "", err
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(context.Background(), remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		return strings.TrimSpace(stderr.String()), err
	}

	return strings.TrimSpace(stdout.String()), nil
}

func takeFullSnapshot(config *rest.Config, clientSet kubernetes.Clientset, podNamespace, podName string, tls bool) (*brtypes.Snapshot, error) {
	command := "curl "
	if tls {
		command += "-k https"
	} else {
		command += "http"
	}
	command += "://localhost:8080/snapshot/full"

	res, err := executeContainerCommand(config, clientSet, podNamespace, podName, debugContainerName, command)
	if err != nil {
		return nil, err
	}

	snapshot := &brtypes.Snapshot{}
	if err = json.Unmarshal([]byte(res), snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func getDataPVCForPod(c client.Client, pod *corev1.Pod) (*corev1.PersistentVolumeClaim, error) {
	for _, volume := range pod.Spec.Volumes {
		if volume.PersistentVolumeClaim != nil {
			pvcName := volume.PersistentVolumeClaim.ClaimName
			pvc := &corev1.PersistentVolumeClaim{}
			if err := c.Get(context.Background(), types.NamespacedName{Name: pvcName, Namespace: pod.Namespace}, pvc); err != nil {
				return nil, fmt.Errorf("unable to fetch pvc: %v", err)
			}
			return pvc, nil
		}
	}
	return nil, fmt.Errorf("no pvcs found for pod")
}

// getVolumeIDForDataPVC returns the AWS volume ID and region for the given data PVC
func getVolumeIDForDataPVC(c client.Client, pvc *corev1.PersistentVolumeClaim, pvNodeMap map[string]string) (string, string, error) {
	pv := &corev1.PersistentVolume{}
	pvName := pvc.Spec.VolumeName
	if err := c.Get(context.Background(), types.NamespacedName{Name: pvName}, pv); err != nil {
		return "", "", fmt.Errorf("unable to fetch pv: %v", err)
	}
	if pv.Spec.AWSElasticBlockStore != nil {
		log.Printf("Found pv.Spec.AWSElasticBlockStore for pv %s\n", pv.Name)
		var region string

		region, ok := pv.Labels["topology.kubernetes.io/region"]
		if ok {
			items := strings.Split(pv.Spec.AWSElasticBlockStore.VolumeID, "/")
			return items[len(items)-1], region, nil
		}
		log.Printf("no region label found for pv %s. Checking node selector terms.", pv.Name)

		if pv.Spec.NodeAffinity == nil || pv.Spec.NodeAffinity.Required == nil || pv.Spec.NodeAffinity.Required.NodeSelectorTerms == nil || len(pv.Spec.NodeAffinity.Required.NodeSelectorTerms) == 0 || pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions == nil || len(pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions) == 0 {
			return "", "", fmt.Errorf("no zone selector found for pv %s", pv.Name)
		}
		for _, matchExpression := range pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions {
			if strings.HasSuffix(matchExpression.Key, "region") {
				region = matchExpression.Values[0]
				log.Printf("Found region %s for pv %s\n", region, pv.Name)
				break
			} else if strings.HasSuffix(matchExpression.Key, "zone") {
				zone := matchExpression.Values[0]
				log.Printf("Found zone %s for pv %s\n", zone, pv.Name)
				region = zone[:len(zone)-1]
			}
		}
		if region == "" {
			return "", "", fmt.Errorf("unable to fetch region from node selector terms for pv %s", pv.Name)
		}

		items := strings.Split(pv.Spec.AWSElasticBlockStore.VolumeID, "/")
		return items[len(items)-1], region, nil
	}
	if pv.Spec.CSI != nil && pv.Spec.CSI.Driver == "ebs.csi.aws.com" {
		log.Printf("Found pv.Spec.CSI for pv %s. Cannot be supported for hibernated shoots.\n", pv.Name)

		nodeName, ok := pvNodeMap[pv.Name]
		if !ok || nodeName == "" {
			return "", "", fmt.Errorf("no node found for pv %s", pv.Name)
		}

		node := &corev1.Node{}
		if err := c.Get(context.Background(), types.NamespacedName{Name: nodeName}, node); err != nil {
			return "", "", fmt.Errorf("unable to fetch node: %v", err)
		}
		region, ok := node.Labels["topology.kubernetes.io/region"]
		if !ok {
			return "", "", fmt.Errorf("no region label found for node %s", node.Name)
		}
		return pv.Spec.CSI.VolumeHandle, region, nil
	}
	return "", "", fmt.Errorf("no AWS volume ID found for pvc %s", pvc.Name)
}

func getVolumeInfoFromAWS(volumeID, volumeRegion string) (*ec2.Volume, error) {
	session, err := session.NewSession(&aws.Config{
		Region: aws.String(volumeRegion),
	})
	if err != nil {
		return nil, err
	}

	svc := ec2.New(session)

	params := &ec2.DescribeVolumesInput{
		VolumeIds: []*string{
			aws.String(volumeID),
		},
	}

	// Retrieve information about the volume
	result, err := svc.DescribeVolumes(params)
	if err != nil {
		return nil, err
	}

	// Check if any volume information was returned
	if len(result.Volumes) == 0 {
		return nil, fmt.Errorf("no volume found with ID %s", volumeID)
	}

	return result.Volumes[0], nil
}

func isEBSVolumeEncrypted(volume *ec2.Volume) bool {
	return volume.Encrypted != nil && pointer.BoolDeref(volume.Encrypted, false)
}

func scaleDeployment(c client.Client, name, namespace string, replicas int32) error {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	if err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, dep); err != nil {
		return fmt.Errorf("unable to get deployment %s/%s", namespace, name)
	}
	log.Printf("Scaling deployment %s/%s from %d to %d replicas", namespace, name, dep.Spec.Replicas, replicas)

	scale := &autoscalingv1.Scale{Spec: autoscalingv1.ScaleSpec{Replicas: replicas}}
	if err := c.SubResource("scale").Update(context.Background(), dep, client.WithSubResourceBody(scale)); err != nil {
		return err
	}

	return gardenerretry.UntilTimeout(context.Background(), 2*time.Second, 2*time.Minute, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, dep); err != nil {
			if apierrors.IsNotFound(err) {
				return gardenerretry.MinorError(err)
			}
			return gardenerretry.SevereError(err)
		}

		if dep.Status.ObservedGeneration != dep.Generation || dep.Status.Replicas != replicas || dep.Status.ReadyReplicas != replicas {
			return gardenerretry.MinorError(fmt.Errorf("deployment not scaled yet"))
		}
		labelRoleValue, ok := dep.Labels[kapiLabelRoleKey]
		if !ok {
			return gardenerretry.SevereError(fmt.Errorf("deployment missing %s label", kapiLabelRoleKey))
		}

		pods := &corev1.PodList{}
		if err := c.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{kapiLabelRoleKey: labelRoleValue}); err != nil {
			if apierrors.IsNotFound(err) {
				if replicas == 0 {
					return gardenerretry.Ok()
				}
				return gardenerretry.MinorError(err)
			}
			return gardenerretry.SevereError(err)
		}
		if len(pods.Items) == int(replicas) {
			return gardenerretry.Ok()
		}
		return gardenerretry.MinorError(fmt.Errorf("deployment not scaled yet"))
	})
}

func scaleDownDeployment(c client.Client, deploymentName, deploymentNamespace string) (int32, error) {
	dep := &appsv1.Deployment{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: deploymentName, Namespace: deploymentNamespace}, dep); err != nil {
		return 0, fmt.Errorf("unable to fetch deployment: %v", err)
	}
	replicas := dep.Spec.Replicas

	return *replicas, scaleDeployment(c, deploymentName, deploymentNamespace, 0)
}

func scaleUpDeployment(c client.Client, deploymentName, deploymentNamespace string, replicas int32) error {
	return scaleDeployment(c, deploymentName, deploymentNamespace, replicas)
}

func getS3Snapstore(bucketName, storePrefix string) (brtypes.SnapStore, error) {
	snapstoreConfig := &brtypes.SnapstoreConfig{
		Provider:  brtypes.SnapstoreProviderS3,
		Container: bucketName,
		Prefix:    path.Join(storePrefix, "v2"),
	}
	store, err := snapstore.GetSnapstore(snapstoreConfig)
	if err != nil {
		return nil, err
	}
	return store, nil
}

func getLatestFullSnapshot(bucketName, storePrefix string) (*brtypes.Snapshot, error) {
	snapstore, err := getS3Snapstore(bucketName, storePrefix)
	if err != nil {
		return nil, err
	}
	snapList, err := snapstore.List()
	if err != nil {
		return nil, err
	}

	if len(snapList) == 0 {
		return nil, fmt.Errorf("no snapshots found")
	}

	fullSnap, deltaSnaps, err := miscellaneous.GetLatestFullSnapshotAndDeltaSnapList(snapstore)
	if err != nil {
		return nil, err
	}

	if deltaSnaps != nil || len(deltaSnaps) > 0 {
		return nil, fmt.Errorf("unexpected delta snapshots exist")
	}

	return fullSnap, nil
}

func deletePod(c client.Client, pod *corev1.Pod) error {
	return c.Delete(context.Background(), pod)
}

func deletePVCInBackground(c client.Client, pvc *corev1.PersistentVolumeClaim) error {
	return c.Delete(context.Background(), pvc, client.PropagationPolicy(metav1.DeletePropagationBackground))
}

func waitForPVCRecreation(c client.Client, pvcNamespace, pvcName, oldPVCUID string) (*corev1.PersistentVolumeClaim, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	err := gardenerretry.UntilTimeout(context.Background(), 2*time.Second, 2*time.Minute, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: pvcNamespace}, pvc); err != nil {
			if apierrors.IsNotFound(err) {
				return gardenerretry.MinorError(err)
			}
			return gardenerretry.SevereError(err)
		}
		if pvc.DeletionTimestamp != nil {
			return gardenerretry.MinorError(fmt.Errorf("old pvc is being deleted"))
		}
		if string(pvc.UID) == oldPVCUID {
			return gardenerretry.MinorError(fmt.Errorf("pvc not recreated yet, since UID %s is the same as old pvc", pvc.UID))
		}
		return gardenerretry.Ok()
	})

	if err != nil {
		return nil, err
	}
	return pvc, nil
}

func waitForPodReady(c client.Client, podName, podNamespace string) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := gardenerretry.UntilTimeout(context.Background(), 2*time.Second, 5*time.Minute, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, types.NamespacedName{Name: podName, Namespace: podNamespace}, pod); err != nil {
			if apierrors.IsNotFound(err) {
				return gardenerretry.MinorError(err)
			}
			return gardenerretry.SevereError(err)
		}

		var podReady, containersReady bool
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady {
				podReady = cond.Status == corev1.ConditionTrue
			}
			if cond.Type == corev1.ContainersReady {
				containersReady = cond.Status == corev1.ConditionTrue
			}
		}
		if podReady && containersReady {
			return gardenerretry.Ok()
		}
		return gardenerretry.MinorError(fmt.Errorf("pod not ready yet"))
	})
	if err != nil {
		return nil, err
	}
	return pod, nil
}

func installEtcdctl(config *rest.Config, clientSet kubernetes.Clientset, podNamespace, podName string) error {
	command := "cd /nonroot/hacks && ./install_etcdctl"
	_, err := executeContainerCommand(config, clientSet, podNamespace, podName, debugContainerName, command)
	if err != nil {
		return err
	}
	return nil
}

func getEtcdWrapperProcessID(config *rest.Config, clientSet kubernetes.Clientset, podNamespace, podName string) (int, error) {
	command := "pidof etcd-wrapper"
	result, err := executeContainerCommand(config, clientSet, podNamespace, podName, debugContainerName, command)
	if err != nil {
		return 0, nil
	}

	return strconv.Atoi(result)
}

func getEtcdctlCommandBase(etcdName string, tls bool, etcdPid int) string {
	command := "./nonroot/hacks/etcdctl"
	if tls {
		command += fmt.Sprintf(" --endpoints=https://%s-local:2379", etcdName)
		command += fmt.Sprintf(" --cacert=/proc/%[1]d/root/var/etcd/ssl/client/ca/bundle.crt --cert=/proc/%[1]d/root/var/etcd/ssl/client/client/tls.crt --key=/proc/%[1]d/root/var/etcd/ssl/client/client/tls.key", etcdPid)
	} else {
		command += fmt.Sprintf(" --endpoints=http://%s-local:2379", etcdName)
	}
	return command
}

type endpointStatusResult struct {
	Endpoint string                      `json:"Endpoint"`
	Status   etcdserverpb.StatusResponse `json:"Status"`
}

func getEtcdctlEndpointStatus(config *rest.Config, clientSet kubernetes.Clientset, podNamespace, podName, etcdName string, tls bool, etcdPid int) (*etcdserverpb.StatusResponse, error) {
	command := getEtcdctlCommandBase(etcdName, tls, etcdPid)
	//command += " endpoint status -w=json | jq '.[0].Status.header.revision'"
	command += " endpoint status -w=json"

	// ./nonroot/hacks/etcdctl --endpoints=https://etcd-main-local:2379 --cacert=/proc/13/root/var/etcd/ssl/client/ca/bundle.crt --cert=/proc/13/root/var/etcd/ssl/client/client/tls.crt --key=/proc/13/root/var/etcd/ssl/client/client/tls.key endpoint status -w=json | jq '.[0].Status.header.revision'

	result, err := executeContainerCommand(config, clientSet, podNamespace, podName, debugContainerName, command)
	if err != nil {
		return nil, err
	}

	var statusResponses []endpointStatusResult
	if err = json.Unmarshal([]byte(result), &statusResponses); err != nil {
		return nil, err
	}

	return &statusResponses[0].Status, nil
}

func getCurrentEtcdRevision(config *rest.Config, clientSet kubernetes.Clientset, podNamespace, podName, etcdName string, tls bool, etcdPid int) (int64, error) {
	statusResponse, err := getEtcdctlEndpointStatus(config, clientSet, podNamespace, podName, etcdName, tls, etcdPid)
	if err != nil {
		return 0, err
	}

	return statusResponse.Header.Revision, nil

	//return strconv.ParseInt(result, 10, 64)
}

func isEtcdLeader(config *rest.Config, clientSet kubernetes.Clientset, podNamespace, podName, etcdName string, tls bool, etcdPid int) (bool, error) {
	statusResponse, err := getEtcdctlEndpointStatus(config, clientSet, podNamespace, podName, etcdName, tls, etcdPid)
	if err != nil {
		return false, err
	}

	return statusResponse.Header.MemberId == statusResponse.Leader, nil
}

func getEtcdMemberID(config *rest.Config, clientSet kubernetes.Clientset, podNamespace, podName, etcdName string, tls bool, etcdPid int) (string, error) {
	statusResponse, err := getEtcdctlEndpointStatus(config, clientSet, podNamespace, podName, etcdName, tls, etcdPid)
	if err != nil {
		return "", err
	}

	return strconv.FormatUint(statusResponse.Header.MemberId, 16), nil
}

func moveLeadershipToMember(config *rest.Config, clientSet kubernetes.Clientset, podNamespace, podName, etcdName string, tls bool, etcdPid int, memberId string) error {
	command := getEtcdctlCommandBase(etcdName, tls, etcdPid)
	command += fmt.Sprintf(" move-leader %s", memberId)

	result, err := executeContainerCommand(config, clientSet, podNamespace, podName, debugContainerName, command)
	if err != nil {
		return err
	}
	log.Println(result)
	return nil
}

func attachEphemeralContainer(c client.Client, clientSet kubernetes.Clientset, namespace string, podName string, targetContainer string) error {
	pod, err := clientSet.CoreV1().Pods(namespace).Get(context.Background(), podName, metav1.GetOptions{})
	if err != nil {
		return err
	}

	if pod.Spec.EphemeralContainers == nil {
		pod.Spec.EphemeralContainers = make([]corev1.EphemeralContainer, 0)
	} else {
		for _, container := range pod.Spec.EphemeralContainers {
			if container.Name == debugContainerName {
				// ephemeral container already exists
				for _, ephContainerStatus := range pod.Status.EphemeralContainerStatuses {
					if ephContainerStatus.Name == debugContainerName {
						if ephContainerStatus.State.Running != nil {
							return nil
						}
					}
				}
				return fmt.Errorf("ephemeral container %s already exists, but not running", debugContainerName)
			}
		}
	}

	log.Printf("Creating ephemeral container %s\n", debugContainerName)
	pod.Spec.EphemeralContainers = append(pod.Spec.EphemeralContainers, corev1.EphemeralContainer{
		TargetContainerName: targetContainer,
		EphemeralContainerCommon: corev1.EphemeralContainerCommon{
			Image:           "europe-docker.pkg.dev/sap-se-gcp-k8s-delivery/releases-public/eu_gcr_io/gardener-project/gardener/ops-toolbelt:0.25.0-mod1",
			Name:            debugContainerName,
			ImagePullPolicy: "IfNotPresent",
			Command:         []string{"/bin/bash", "-c", "--"},
			Args:            []string{"trap : TERM INT; sleep 9999999999d & wait"},
		},
	})
	_, err = clientSet.CoreV1().Pods(namespace).UpdateEphemeralContainers(context.Background(), podName, pod, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("unable to create ephemeral container: %v", err)
	}

	return gardenerretry.UntilTimeout(context.Background(), 2*time.Second, 1*time.Minute, func(ctx context.Context) (bool, error) {
		if err := c.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, pod); err != nil {
			if apierrors.IsNotFound(err) {
				return gardenerretry.MinorError(err)
			}
			return gardenerretry.SevereError(err)
		}
		for _, ephContainerStatus := range pod.Status.EphemeralContainerStatuses {
			if ephContainerStatus.Name == debugContainerName {
				if ephContainerStatus.State.Running != nil {
					return gardenerretry.Ok()
				}
			}
		}
		return gardenerretry.MinorError(fmt.Errorf("ephemeral container not running yet"))
	})
}

type runnerConfig struct {
	kubeconfigPath                string
	dryRun                        bool
	scaleKubeApiserver            bool
	replaceOnlyUnencryptedVolumes bool
	tlsEnabled                    bool
	outputFilePath                string
	clusterName                   string
	landscapeName                 string
	summarize                     bool
}

type runner struct {
	config runnerConfig
}

func (r *runner) addFlags() {
	flag.StringVar(&r.config.kubeconfigPath, "kubeconfig", os.Getenv(KUBECONFIG), "Path to kubeconfig file. Default: value of KUBECONFIG environment variable.")
	flag.BoolVar(&r.config.dryRun, "dry-run", true, "Indicates whether to perform volume deletions or simply print etcd information.")
	flag.BoolVar(&r.config.scaleKubeApiserver, "scale-kube-apiserver", true, "Indicates whether to scale the kube-apiserver deployment.")
	flag.BoolVar(&r.config.replaceOnlyUnencryptedVolumes, "replace-only-unencrypted-volumes", true, "Indicates whether to replace only unencrypted volumes or all volumes.")
	flag.BoolVar(&r.config.tlsEnabled, "tls-enabled", true, "Indicates whether TLS is enabled for the etcd and backup-restore processes.")
	flag.StringVar(&r.config.outputFilePath, "output-file", "", "Path to json file to write output to. If file does not exist, one will be created. If flag is not passed, a random file will be created in the current directory for this purpose.")
	flag.StringVar(&r.config.landscapeName, "landscape", "", "Name of the landscape the script is run on.")
	flag.BoolVar(&r.config.summarize, "summarize", false, "Is set to true, a summary of the json file specified by --output-file will be printed out.")
}

func (r *runner) validateConfig() error {
	if r.config.summarize {
		if r.config.outputFilePath == "" {
			return fmt.Errorf("output file path is required when summarize is set to true")
		}
		return nil
	}

	if r.config.kubeconfigPath == "" {
		return fmt.Errorf("kubeconfig unavailable from either env var or flag")
	}
	config, err := getRawConfig(r.config.kubeconfigPath)
	if err != nil {
		return fmt.Errorf("unable to load kubeconfig: %v", err)
	}
	users := config.AuthInfos
	if len(users) != 1 {
		return fmt.Errorf("expected 1 user in kubeconfig, but got %d", len(users))
	}
	for name, _ := range users {
		r.config.clusterName = strings.TrimPrefix(name, "garden--")
	}

	if r.config.outputFilePath == "" {
		r.config.outputFilePath = fmt.Sprintf("output-%s.json", time.Now().Format("2006-01-02-15-04-05"))
	}
	if r.config.landscapeName == "" {
		return fmt.Errorf("landscape name is required")
	}
	return nil
}

func getRawConfig(kubeconfigPath string) (api.Config, error) {
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfigPath},
		&clientcmd.ConfigOverrides{
			CurrentContext: "",
		}).RawConfig()
}

func getConfigAndClient(kubeconfigPath string) (*rest.Config, client.Client, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, nil, fmt.Errorf("unable to build config: %v", err)
	}

	var scheme = runtime.NewScheme()
	scheme.AddKnownTypes(v1alpha1.GroupVersion)

	c, err := client.New(config, client.Options{
		Scheme: druidkubernetes.Scheme,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("unable to create client: %v", err)
	}

	return config, c, nil
}

func getClientSet(config *rest.Config) (*kubernetes.Clientset, error) {
	return kubernetes.NewForConfig(config)
}

func (r *runner) getEtcdFullName(etcd *v1alpha1.Etcd) string {
	return fmt.Sprintf("%s/%s/%s/%s", r.config.landscapeName, r.config.clusterName, etcd.Namespace, etcd.Name)
}

func (r *runner) handleSingleNode(c client.Client, clientSet *kubernetes.Clientset, config *rest.Config, etcd *v1alpha1.Etcd, pvNodeMap map[string]string, unencryptedVolumes *[]*volume) error {
	log.Printf("Fetching statefulset labels for etcd %s/%s\n", etcd.Namespace, etcd.Name)
	stsLabels, err := getEtcdStatefulSetLabels(c, etcd)
	if err != nil {
		return fmt.Errorf("unable to get etcd statefulset labels: %v", err)
	}
	log.Printf("Fetched statefulset labels for etcd %s/%s: %v\n", etcd.Namespace, etcd.Name, stsLabels)

	log.Printf("Fetching PVCs for etcd %s/%s\n", etcd.Namespace, etcd.Name)
	pvcs, err := getPVCsForEtcd(c, etcd, stsLabels)
	if err != nil {
		return fmt.Errorf("unable to get pvcs for etcd: %v", err)
	}
	if len(pvcs) != 1 {
		return fmt.Errorf("expected 1 pvc for etcd, but got %d", len(pvcs))
	}
	pvc := &pvcs[0]
	log.Printf("Fetched PVC %s for etcd %s/%s\n", pvc.Name, etcd.Namespace, etcd.Name)

	log.Printf("Fetching volume ID for pvc %s/%s\n", pvc.Namespace, pvc.Name)
	volumeID, volumeRegion, err := getVolumeIDForDataPVC(c, pvc, pvNodeMap)
	if err != nil {
		return fmt.Errorf("unable to get volume ID for pvc %s/%s: %v", pvc.Namespace, pvc.Name, err)
	}
	log.Printf("Fetched volume ID %s in region %s\n", volumeID, volumeRegion)

	log.Printf("Fetching volume info for volume ID %s in region %s\n", volumeID, volumeRegion)
	volumeInfo, err := getVolumeInfoFromAWS(volumeID, volumeRegion)
	if err != nil {
		return fmt.Errorf("unable to get volume info from AWS for volume ID %s in region %s: %v", volumeID, volumeRegion, err)
	}
	log.Printf("Fetched volume info for %s: encrypted: %t, volumeType: %s, createTime: %v\n", *volumeInfo.VolumeId, *volumeInfo.Encrypted, *volumeInfo.VolumeType, *volumeInfo.CreateTime)
	if len(volumeInfo.Attachments) != 0 {
		log.Printf("Volume %s is attached to node %s\n", *volumeInfo.VolumeId, *volumeInfo.Attachments[0].InstanceId)
	}
	// TODO: enable check again
	//if !isEBSVolumeEncrypted(volumeInfo) {
	if !isVolumeInVolumes(volumeID, *unencryptedVolumes) {
		*unencryptedVolumes = append(*unencryptedVolumes, &volume{
			Cluster:      fmt.Sprintf("%s/%s", r.config.landscapeName, r.config.clusterName),
			Namespace:    etcd.Namespace,
			Etcd:         etcd.Name,
			EtcdReplicas: 1,
			PVC:          pvc.Name,
			VolumeID:     volumeID,
			Region:       volumeRegion,
			Encrypted:    pointer.BoolDeref(volumeInfo.Encrypted, false),
		})
	}
	//}

	if r.config.dryRun {
		log.Printf("Dry-run mode enabled, skipping volume replacement for etcd %s/%s\n", etcd.Namespace, etcd.Name)
		return nil
	}

	if !r.config.replaceOnlyUnencryptedVolumes || !isEBSVolumeEncrypted(volumeInfo) {
		log.Printf("Will replace volume %s for etcd %s/%s because replaceOnlyUnencryptedVolumes set to %t and volume encryption is %t \n", volumeID, etcd.Namespace, etcd.Name, r.config.replaceOnlyUnencryptedVolumes, isEBSVolumeEncrypted(volumeInfo))

		isShootHibernated := isEtcdScaledDown(etcd)
		if isShootHibernated {
			log.Printf("Shoot is hibernated, scaling up etcd %s/%s to 1 replica\n", etcd.Namespace, etcd.Name)
			if err = scaleUpEtcd(c, etcd, 1, stsLabels); err != nil {
				return fmt.Errorf("unable to scale up etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
			}
			log.Printf("Scaled up etcd %s/%s to 1 replica\n", etcd.Namespace, etcd.Name)
		}

		pods, err := getEtcdPods(c, etcd, stsLabels)
		if err != nil {
			return fmt.Errorf("unable to get pods for etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
		}
		if len(pods) != 1 {
			return fmt.Errorf("expected 1 pod for etcd %s/%s, but got %d", etcd.Namespace, etcd.Name, len(pods))
		}
		pod := &pods[0]

		log.Printf("Attaching ephemeral container %s to pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)
		if err = attachEphemeralContainer(c, *clientSet, pod.Namespace, pod.Name, "etcd"); err != nil {
			return fmt.Errorf("unable to attach ephemeral container to pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Attached ephemeral container %s to pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)

		log.Printf("Installing etcdctl in container %s in pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)
		if err = installEtcdctl(config, *clientSet, pod.Namespace, pod.Name); err != nil {
			return fmt.Errorf("unable to install etcdctl in pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Installed etcdctl in container %s in pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)

		kapiReplicas := int32(0)
		if !isShootHibernated && r.config.scaleKubeApiserver {
			log.Printf("Scaling down deployment %s/%s\n", etcd.Namespace, kapiDeploymentName)
			kapiReplicas, err = scaleDownDeployment(c, kapiDeploymentName, etcd.Namespace)
			if err != nil {
				return fmt.Errorf("unable to scale down kapi %s/%s: %v", etcd.Namespace, kapiDeploymentName, err)
			}
			log.Printf("Scaled down deployment %s/%s from %d to 0 replicas\n", etcd.Namespace, kapiDeploymentName, kapiReplicas)
		}

		log.Printf("Getting etcd-wrapper process ID for pod %s/%s\n", pod.Namespace, pod.Name)
		etcdPID, err := getEtcdWrapperProcessID(config, *clientSet, pod.Namespace, pod.Name)
		if err != nil {
			return fmt.Errorf("unable to get etcd-wrapper pid for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Got etcd-wrapper process ID %d for pod %s/%s\n", etcdPID, pod.Namespace, pod.Name)

		log.Printf("Getting old etcd revision for pod %s/%s\n", pod.Namespace, pod.Name)
		oldRevision, err := getCurrentEtcdRevision(config, *clientSet, pod.Namespace, pod.Name, etcd.Name, r.config.tlsEnabled, etcdPID)
		if err != nil {
			return fmt.Errorf("unable to get old etcd revision for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Got old etcd revision %d for pod %s/%s\n", oldRevision, pod.Namespace, pod.Name)

		log.Printf("Taking full snapshot for pod %s/%s\n", pod.Namespace, pod.Name)
		fullSnapshotExpected, err := takeFullSnapshot(config, *clientSet, pod.Namespace, pod.Name, r.config.tlsEnabled)
		if err != nil {
			return fmt.Errorf("unable to take full snapshot for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		if fullSnapshotExpected == nil {
			return fmt.Errorf("full snapshot for pod %s/%s is nil", pod.Namespace, pod.Name)
		}
		if fullSnapshotExpected.LastRevision != oldRevision {
			return fmt.Errorf("full snapshot %s for pod %s/%s was taken at revision %d but etcd revision is at %d", fullSnapshotExpected.SnapName, pod.Namespace, pod.Name, fullSnapshotExpected.LastRevision, oldRevision)
		}
		log.Printf("Took full snapshot %s for pod %s/%s\n", fullSnapshotExpected.SnapName, pod.Namespace, pod.Name)

		log.Printf("Ensuring latest full snapshot for etcd %s/%s is uploaded correctly\n", etcd.Namespace, etcd.Name)
		log.Printf("Fetching latest full snapshot for etcd %s/%s\n", etcd.Namespace, etcd.Name)
		fullSnapshotObserved, err := getLatestFullSnapshot(*etcd.Spec.Backup.Store.Container, etcd.Spec.Backup.Store.Prefix)
		if err != nil {
			return fmt.Errorf("unable to check latest full snapshot for etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
		}
		log.Printf("Fetched latest full snapshot %s for etcd %s/%s\n", fullSnapshotObserved.SnapName, etcd.Namespace, etcd.Name)

		log.Printf("Comparing observed latest snapshot %s with expected uploaded snapshot %s\n", fullSnapshotObserved.SnapName, fullSnapshotExpected.SnapName)
		if fullSnapshotObserved.LastRevision != fullSnapshotExpected.LastRevision {
			return fmt.Errorf("found latest snapshot %+v\n but expected uploaded snapshot %+v", fullSnapshotObserved, fullSnapshotExpected)
		}
		log.Printf("Observed latest snapshot %s matches expected uploaded snapshot %s, with revision %d\n", fullSnapshotObserved.SnapName, fullSnapshotExpected.SnapName, fullSnapshotObserved.LastRevision)

		log.Printf("Issuing deletion call to PVC %s/%s in background\n", pvc.Namespace, pvc.Name)
		if err = deletePVCInBackground(c, pvc); err != nil {
			return fmt.Errorf("unable to delete pvc %s/%s in background: %v", pvc.Namespace, pvc.Name, err)
		}
		log.Printf("Issued deletion call to PVC %s/%s in background\n", pvc.Namespace, pvc.Name)

		log.Printf("Deleting pod %s/%s\n", pod.Namespace, pod.Name)
		if err = deletePod(c, pod); err != nil {
			return fmt.Errorf("unable to delete pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Deleted pod %s/%s\n", pod.Namespace, pod.Name)

		log.Printf("Waiting for PVC %s/%s to be recreated\n", pvc.Namespace, pvc.Name)
		pvc, err = waitForPVCRecreation(c, pvc.Namespace, pvc.Name, string(pvc.UID))
		if err != nil {
			return fmt.Errorf("pvc %s/%s was not recreated in time: %v", pvc.Namespace, pvc.Name, err)
		}
		log.Printf("PVC %s/%s was recreated\n", pvc.Namespace, pvc.Name)

		log.Printf("Waiting for pod %s/%s to become ready\n", pod.Namespace, pod.Name)
		pod, err = waitForPodReady(c, pod.Name, pod.Namespace)
		if err != nil {
			return fmt.Errorf("pod %s/%s did not become ready in time: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Pod %s/%s became ready\n", pod.Namespace, pod.Name)

		log.Printf("Attaching ephemeral container %s to pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)
		if err = attachEphemeralContainer(c, *clientSet, pod.Namespace, pod.Name, "etcd"); err != nil {
			return fmt.Errorf("unable to attach ephemeral container to pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Attached ephemeral container %s to pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)

		log.Printf("Installing etcdctl in container %s in pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)
		if err = installEtcdctl(config, *clientSet, pod.Namespace, pod.Name); err != nil {
			return fmt.Errorf("unable to install etcdctl in pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Installed etcdctl in container %s in pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)

		log.Printf("Getting etcd-wrapper process ID for pod %s/%s\n", pod.Namespace, pod.Name)
		etcdPID, err = getEtcdWrapperProcessID(config, *clientSet, pod.Namespace, pod.Name)
		if err != nil {
			return fmt.Errorf("unable to get etcd-wrapper pid for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Got etcd-wrapper process ID %d for pod %s/%s\n", etcdPID, pod.Namespace, pod.Name)

		log.Printf("Getting new etcd revision for pod %s/%s\n", pod.Namespace, pod.Name)
		newRevision, err := getCurrentEtcdRevision(config, *clientSet, pod.Namespace, pod.Name, etcd.Name, r.config.tlsEnabled, etcdPID)
		if err != nil {
			return fmt.Errorf("unable to get new etcd revision for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Got new etcd revision %d for pod %s/%s\n", newRevision, pod.Namespace, pod.Name)

		log.Printf("Ensuring new etcd revision %d matches old etcd revision %d\n", newRevision, oldRevision)
		if newRevision != oldRevision {
			return fmt.Errorf("new revision %d does not match old revision %d", newRevision, oldRevision)
		}
		log.Printf("New etcd revision %d matches old etcd revision %d\n", newRevision, oldRevision)

		if !isShootHibernated && r.config.scaleKubeApiserver {
			log.Printf("Scaling up deployment %s/%s\n", etcd.Namespace, kapiDeploymentName)
			if err = scaleUpDeployment(c, kapiDeploymentName, etcd.Namespace, kapiReplicas); err != nil {
				return fmt.Errorf("unable to scale up kapi %s/%s: %v", etcd.Namespace, kapiDeploymentName, err)
			}
			log.Printf("Scaled up deployment %s/%s from 0 to %d replicas\n", etcd.Namespace, kapiDeploymentName, kapiReplicas)
		}

		if isShootHibernated {
			log.Printf("Shoot %s is hibernated, scaling down etcd %s/%s now\n", etcd.Namespace, etcd.Namespace, etcd.Name)
			if err = scaleDownEtcd(c, etcd, stsLabels); err != nil {
				return fmt.Errorf("unable to scale down etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
			}
			log.Printf("Scaled down etcd %s/%s\n", etcd.Namespace, etcd.Name)
		}

		log.Printf("etcd %s/%s successfully processed\n", etcd.Namespace, etcd.Name)
	} else {
		log.Printf("Volume ID %s for etcd %s/%s is already encrypted.\n", volumeID, etcd.Namespace, etcd.Name)
	}
	return nil
}

func (r *runner) handleMultiNode(c client.Client, clientSet *kubernetes.Clientset, config *rest.Config, etcd *v1alpha1.Etcd, pvNodeMap map[string]string, unencryptedVolumes *[]*volume) error {
	var (
		pvcs                []corev1.PersistentVolumeClaim
		volumeInfos         []*ec2.Volume
		allVolumesEncrypted = true
	)

	log.Printf("Fetching statefulset labels for etcd %s/%s\n", etcd.Namespace, etcd.Name)
	stsLabels, err := getEtcdStatefulSetLabels(c, etcd)
	if err != nil {
		return fmt.Errorf("unable to get etcd statefulset labels: %v", err)
	}
	log.Printf("Fetched statefulset labels for etcd %s/%s: %v\n", etcd.Namespace, etcd.Name, stsLabels)

	log.Printf("Fetching PVCs for etcd %s/%s\n", etcd.Namespace, etcd.Name)
	pvcs, err = getPVCsForEtcd(c, etcd, stsLabels)
	if err != nil {
		return fmt.Errorf("unable to get pvcs for etcd: %v", err)
	}
	if len(pvcs) != 3 {
		return fmt.Errorf("expected 3 pvcs for etcd, but got %d", len(pvcs))
	}
	msg := "Fetched PVCs: "
	for i, pvc := range pvcs {
		if i == len(pvcs)-1 {
			msg += fmt.Sprintf("%s\n", pvc.Name)
		} else {
			msg += fmt.Sprintf("%s, ", pvc.Name)
		}
	}
	log.Printf(msg)

	for _, pvc := range pvcs {
		log.Printf("Fetching volume ID for pvc %s/%s\n", pvc.Namespace, pvc.Name)
		volumeID, volumeRegion, err := getVolumeIDForDataPVC(c, &pvc, pvNodeMap)
		if err != nil {
			return fmt.Errorf("unable to get volume ID for pvc %s/%s: %v", pvc.Namespace, pvc.Name, err)
		}
		log.Printf("Fetched volume ID %s in region %s\n", volumeID, volumeRegion)

		log.Printf("Fetching volume info for volume ID %s in region %s\n", volumeID, volumeRegion)
		volumeInfo, err := getVolumeInfoFromAWS(volumeID, volumeRegion)
		if err != nil {
			return fmt.Errorf("unable to get volume info from AWS for volume ID %s in region %s: %v", volumeID, volumeRegion, err)
		}
		log.Printf("Fetched volume info for %s: encrypted: %t, volumeType: %s, createTime: %v\n", *volumeInfo.VolumeId, *volumeInfo.Encrypted, *volumeInfo.VolumeType, *volumeInfo.CreateTime)
		if len(volumeInfo.Attachments) != 0 {
			log.Printf("Volume %s is attached to node %s\n", *volumeInfo.VolumeId, *volumeInfo.Attachments[0].InstanceId)
		}
		// TODO: enable check again
		//if !isEBSVolumeEncrypted(volumeInfo) {
		if !isVolumeInVolumes(volumeID, *unencryptedVolumes) {
			*unencryptedVolumes = append(*unencryptedVolumes, &volume{
				Cluster:      fmt.Sprintf("%s/%s", r.config.landscapeName, r.config.clusterName),
				Namespace:    etcd.Namespace,
				Etcd:         etcd.Name,
				EtcdReplicas: 3,
				PVC:          pvc.Name,
				VolumeID:     volumeID,
				Region:       volumeRegion,
				Encrypted:    pointer.BoolDeref(volumeInfo.Encrypted, false),
			})
		}
		//}

		volumeInfos = append(volumeInfos, volumeInfo)
		allVolumesEncrypted = allVolumesEncrypted && isEBSVolumeEncrypted(volumeInfo)
	}

	if r.config.dryRun {
		log.Printf("Dry-run mode enabled, skipping volume replacement for etcd %s/%s\n", etcd.Namespace, etcd.Name)
		return nil
	}

	if r.config.replaceOnlyUnencryptedVolumes && allVolumesEncrypted {
		log.Printf("All volumes for etcd %s/%s are already encrypted.\n", etcd.Namespace, etcd.Name)
		return nil
	}

	isShootHibernated := isEtcdScaledDown(etcd)
	if isShootHibernated {
		log.Printf("Shoot is hibernated, scaling up etcd %s/%s to 3 replicas\n", etcd.Namespace, etcd.Name)
		if err = scaleUpEtcd(c, etcd, 3, stsLabels); err != nil {
			return fmt.Errorf("unable to scale up etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
		}
		log.Printf("Scaled up etcd %s/%s to 3 replicas\n", etcd.Namespace, etcd.Name)
	}

	var (
		leaderPodName string
		etcdPIDs      = make(map[string]int)
	)

	pods, err := getEtcdPods(c, etcd, stsLabels)
	if err != nil {
		return fmt.Errorf("unable to get pods for etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
	}
	if len(pods) != 3 {
		return fmt.Errorf("expected 3 pods for etcd %s/%s, but got %d", etcd.Namespace, etcd.Name, len(pods))
	}

	for _, pod := range pods {
		log.Printf("Attaching ephemeral container %s to pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)
		if err = attachEphemeralContainer(c, *clientSet, pod.Namespace, pod.Name, "etcd"); err != nil {
			return fmt.Errorf("unable to attach ephemeral container to pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Attached ephemeral container %s to pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)

		log.Printf("Installing etcdctl in container %s in pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)
		if err = installEtcdctl(config, *clientSet, pod.Namespace, pod.Name); err != nil {
			return fmt.Errorf("unable to install etcdctl in pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Installed etcdctl in container %s in pod %s/%s\n", debugContainerName, pod.Namespace, pod.Name)

		log.Printf("Getting etcd-wrapper process ID for pod %s/%s\n", pod.Namespace, pod.Name)
		etcdPID, err := getEtcdWrapperProcessID(config, *clientSet, pod.Namespace, pod.Name)
		if err != nil {
			return fmt.Errorf("unable to get etcd-wrapper pid for pod %s/%s: %v", pod.Namespace, pod.Name, err)
		}
		log.Printf("Got etcd-wrapper process ID %d for pod %s/%s\n", etcdPID, pod.Namespace, pod.Name)
		etcdPIDs[pod.Name] = etcdPID

		isLeader, err := isEtcdLeader(config, *clientSet, pod.Namespace, pod.Name, etcd.Name, r.config.tlsEnabled, etcdPID)
		if err != nil {
			return fmt.Errorf("unable to check if pod %s/%s is leader: %v", pod.Namespace, pod.Name, err)
		}
		if isLeader {
			leaderPodName = pod.Name
		}
	}

	log.Printf("Getting old etcd revision for pod %s/%s\n", etcd.Namespace, leaderPodName)
	oldRevision, err := getCurrentEtcdRevision(config, *clientSet, etcd.Namespace, leaderPodName, etcd.Name, r.config.tlsEnabled, etcdPIDs[leaderPodName])
	if err != nil {
		return fmt.Errorf("unable to get old etcd revision for pod %s/%s: %v", etcd.Namespace, leaderPodName, err)
	}
	log.Printf("Got old etcd revision %d for pod %s/%s\n", oldRevision, etcd.Namespace, leaderPodName)

	log.Printf("Taking full snapshot for pod %s/%s\n", etcd.Namespace, leaderPodName)
	fullSnapshotExpected, err := takeFullSnapshot(config, *clientSet, etcd.Namespace, leaderPodName, r.config.tlsEnabled)
	if err != nil {
		return fmt.Errorf("unable to take full snapshot for pod %s/%s: %v", etcd.Namespace, leaderPodName, err)
	}
	if fullSnapshotExpected == nil {
		return fmt.Errorf("full snapshot for pod %s/%s is nil", etcd.Namespace, leaderPodName)
	}
	log.Printf("Took full snapshot %s for pod %s/%s\n", fullSnapshotExpected.SnapName, etcd.Namespace, leaderPodName)

	log.Printf("Ensuring latest full snapshot for etcd %s/%s is uploaded correctly\n", etcd.Namespace, etcd.Name)
	log.Printf("Fetching latest full snapshot for etcd %s/%s\n", etcd.Namespace, etcd.Name)
	fullSnapshotObserved, err := getLatestFullSnapshot(*etcd.Spec.Backup.Store.Container, etcd.Spec.Backup.Store.Prefix)
	if err != nil {
		return fmt.Errorf("unable to check latest full snapshot for etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
	}
	log.Printf("Fetched latest full snapshot %s for etcd %s/%s\n", fullSnapshotObserved.SnapName, etcd.Namespace, etcd.Name)

	log.Printf("Comparing observed latest snapshot %s with expected uploaded snapshot %s\n", fullSnapshotObserved.SnapName, fullSnapshotExpected.SnapName)
	if fullSnapshotObserved.LastRevision != fullSnapshotExpected.LastRevision {
		return fmt.Errorf("found latest snapshot %+v\n but expected uploaded snapshot %+v", fullSnapshotObserved, fullSnapshotExpected)
	}
	log.Printf("Observed latest snapshot %s matches expected uploaded snapshot %s, with revision %d\n", fullSnapshotObserved.SnapName, fullSnapshotExpected.SnapName, fullSnapshotObserved.LastRevision)

	for i := len(pods) - 1; i >= 0; i-- {
		if err = ensureAllMembersReady(c, etcd, len(pods)); err != nil {
			return fmt.Errorf("not all etcd members ready for etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
		}

		if r.config.replaceOnlyUnencryptedVolumes && isEBSVolumeEncrypted(volumeInfos[i]) {
			log.Printf("Volume ID %s for pod %s/%s is already encrypted. Continuing to next pod...\n", *volumeInfos[i].VolumeId, pods[i].Namespace, pods[i].Name)
			continue
		}

		if pods[i].Name == leaderPodName {
			newLeaderPodIndex := i + 1
			if newLeaderPodIndex >= len(pods) {
				newLeaderPodIndex = 0
			}

			log.Printf("Fetching member ID for new leader %s/%s\n", pods[newLeaderPodIndex].Namespace, pods[newLeaderPodIndex].Name)
			newLeaderMemberID, err := getEtcdMemberID(config, *clientSet, pods[newLeaderPodIndex].Namespace, pods[newLeaderPodIndex].Name, etcd.Name, r.config.tlsEnabled, etcdPIDs[pods[newLeaderPodIndex].Name])
			if err != nil {
				return fmt.Errorf("unable to get member ID for pod %s/%s: %v", pods[newLeaderPodIndex].Namespace, pods[newLeaderPodIndex].Name, err)
			}
			log.Printf("Fetched member ID %s for new leader %s/%s\n", newLeaderMemberID, pods[newLeaderPodIndex].Namespace, pods[newLeaderPodIndex].Name)

			log.Printf("Pod %s/%s is current leader. Moving leadership to %s with member ID %s\n", pods[i].Namespace, pods[i].Name, pods[newLeaderPodIndex].Name, newLeaderMemberID)
			if err = moveLeadershipToMember(config, *clientSet, pods[i].Namespace, pods[i].Name, etcd.Name, r.config.tlsEnabled, etcdPIDs[pods[i].Name], newLeaderMemberID); err != nil {
				return fmt.Errorf("unable to move leadership to member %s via pod %s/%s: %v", pods[newLeaderPodIndex].Name, pods[i].Namespace, pods[i].Name, err)
			}
			leaderPodName = pods[newLeaderPodIndex].Name
			log.Printf("Moved leadership to member %s via pod %s/%s\n", leaderPodName, pods[i].Namespace, pods[i].Name)

			if err = ensureAllMembersReady(c, etcd, len(pods)); err != nil {
				return fmt.Errorf("not all etcd members ready for etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
			}
		}

		log.Printf("Issuing deletion call to PVC %s/%s in background\n", pvcs[i].Namespace, pvcs[i].Name)
		if err = deletePVCInBackground(c, &pvcs[i]); err != nil {
			return fmt.Errorf("unable to delete pvc %s/%s in background: %v", pvcs[i].Namespace, pvcs[i].Name, err)
		}
		log.Printf("Issued deletion call to PVC %s/%s in background\n", pvcs[i].Namespace, pvcs[i].Name)

		log.Printf("Deleting pod %s/%s\n", pods[i].Namespace, pods[i].Name)
		if err = deletePod(c, &pods[i]); err != nil {
			return fmt.Errorf("unable to delete pod %s/%s: %v", pods[i].Namespace, pods[i].Name, err)
		}
		log.Printf("Deleted pod %s/%s\n", pods[i].Namespace, pods[i].Name)

		log.Printf("Waiting for PVC %s/%s to be recreated\n", pvcs[i].Namespace, pvcs[i].Name)
		pvc, err := waitForPVCRecreation(c, pvcs[i].Namespace, pvcs[i].Name, string(pvcs[i].UID))
		if err != nil {
			return fmt.Errorf("pvc %s/%s was not recreated in time: %v", pvcs[i].Namespace, pvcs[i].Name, err)
		}
		pvcs[i] = *pvc
		log.Printf("PVC %s/%s was recreated\n", pvcs[i].Namespace, pvcs[i].Name)

		log.Printf("Waiting for pod %s/%s to become ready\n", pods[i].Namespace, pods[i].Name)
		p, err := waitForPodReady(c, pods[i].Name, pods[i].Namespace)
		if err != nil {
			return fmt.Errorf("pod %s/%s did not become ready in time: %v", pods[i].Namespace, pods[i].Name, err)
		}
		pods[i] = *p
		log.Printf("Pod %s/%s became ready\n", pods[i].Namespace, pods[i].Name)

		log.Printf("Attaching ephemeral container %s to pod %s/%s\n", debugContainerName, pods[i].Namespace, pods[i].Name)
		if err = attachEphemeralContainer(c, *clientSet, pods[i].Namespace, pods[i].Name, "etcd"); err != nil {
			return fmt.Errorf("unable to attach ephemeral container to pod %s/%s: %v", pods[i].Namespace, pods[i].Name, err)
		}
		log.Printf("Attached ephemeral container %s to pod %s/%s\n", debugContainerName, pods[i].Namespace, pods[i].Name)

		log.Printf("Installing etcdctl in container %s in pod %s/%s\n", debugContainerName, pods[i].Namespace, pods[i].Name)
		if err = installEtcdctl(config, *clientSet, pods[i].Namespace, pods[i].Name); err != nil {
			return fmt.Errorf("unable to install etcdctl in pod %s/%s: %v", pods[i].Namespace, pods[i].Name, err)
		}
		log.Printf("Installed etcdctl in container %s in pod %s/%s\n", debugContainerName, pods[i].Namespace, pods[i].Name)

		log.Printf("Getting etcd-wrapper process ID for pod %s/%s\n", pods[i].Namespace, pods[i].Name)
		etcdPID, err := getEtcdWrapperProcessID(config, *clientSet, pods[i].Namespace, pods[i].Name)
		if err != nil {
			return fmt.Errorf("unable to get etcd-wrapper pid for pod %s/%s: %v", pods[i].Namespace, pods[i].Name, err)
		}
		etcdPIDs[pods[i].Name] = etcdPID
		log.Printf("Got etcd-wrapper process ID %d for pod %s/%s\n", etcdPID, pods[i].Namespace, pods[i].Name)

		log.Printf("Getting new etcd revision for pod %s/%s\n", pods[i].Namespace, pods[i].Name)
		newRevision, err := getCurrentEtcdRevision(config, *clientSet, pods[i].Namespace, pods[i].Name, etcd.Name, r.config.tlsEnabled, etcdPID)
		if err != nil {
			return fmt.Errorf("unable to get new etcd revision for pod %s/%s: %v", pods[i].Namespace, pods[i].Name, err)
		}
		log.Printf("Got new etcd revision %d for pod %s/%s\n", newRevision, pods[i].Namespace, pods[i].Name)

		log.Printf("Ensuring new etcd revision %d is greater than old etcd revision %d\n", newRevision, oldRevision)
		if newRevision < oldRevision {
			return fmt.Errorf("new revision %d is lesser than old revision %d", newRevision, oldRevision)
		}
		log.Printf("New etcd revision %d is greater than old etcd revision %d\n", newRevision, oldRevision)
	}

	if isShootHibernated {
		log.Printf("Shoot %s is hibernated, scaling down etcd %s/%s now\n", etcd.Namespace, etcd.Namespace, etcd.Name)
		if err = scaleDownEtcd(c, etcd, stsLabels); err != nil {
			return fmt.Errorf("unable to scale down etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
		}
		log.Printf("Scaled down etcd %s/%s\n", etcd.Namespace, etcd.Name)
	}
	log.Printf("etcd %s/%s successfully processed\n", etcd.Namespace, etcd.Name)

	return nil
}

type volume struct {
	Cluster      string `json:"cluster"`
	Namespace    string `json:"namespace"`
	Etcd         string `json:"etcd"`
	EtcdReplicas int    `json:"etcdReplicas"`
	PVC          string `json:"pvc"`
	VolumeID     string `json:"volumeID"`
	Region       string `json:"region"`
	Encrypted    bool   `json:"encrypted"`
}

func getVolumesFromFile(filePath string) ([]*volume, error) {
	var volumes []*volume
	contents, err := os.ReadFile(filePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return volumes, nil
		}
		return nil, fmt.Errorf("unable to read file %s: %v", filePath, err)
	}
	if len(contents) == 0 {
		contents = []byte("[]")
	}
	if err = json.Unmarshal(contents, &volumes); err != nil {
		return nil, fmt.Errorf("unable to unmarshal file %s: %v", filePath, err)
	}
	return volumes, nil
}

func isVolumeInVolumes(volumeID string, volumes []*volume) bool {
	for _, v := range volumes {
		if v.VolumeID == volumeID {
			return true
		}
	}
	return false
}

func (r *runner) run() error {
	var (
		config             *rest.Config
		c                  client.Client
		clientSet          *kubernetes.Clientset
		unencryptedVolumes []*volume
		err                error
	)

	config, c, err = getConfigAndClient(r.config.kubeconfigPath)
	if err != nil {
		return fmt.Errorf("unable to get config and client for cluster: %v", err)
	}

	clientSet, err = getClientSet(config)
	if err != nil {
		return fmt.Errorf("unable to get clientSet for cluster: %v", err)
	}

	etcds, err := getEtcds(c, "etcd-main")
	if err != nil {
		return fmt.Errorf("unable to get etcds: %v", err)
	}
	if len(etcds) == 0 {
		log.Printf("No etcds found\n")
		return nil
	}

	log.Printf("Fetching volume attachments\n")
	volumeAttachments := &storagev1.VolumeAttachmentList{}
	if err := c.List(context.Background(), volumeAttachments); err != nil {
		return fmt.Errorf("unable to fetch volume attachments: %v", err)
	}
	pvNodeMap := make(map[string]string, len(volumeAttachments.Items))
	for _, va := range volumeAttachments.Items {
		if va.Status.Attached {
			pvNodeMap[*va.Spec.Source.PersistentVolumeName] = va.Spec.NodeName
		}
	}
	log.Printf("Fetched volume attachments\n")

	unencryptedVolumes, err = getVolumesFromFile(r.config.outputFilePath)
	if err != nil {
		return fmt.Errorf("unable to get unencrypted volumes from file: %v", err)
	}
	log.Printf("Current unencrypted volume count: %d\n", len(unencryptedVolumes))

	defer func() {
		contents, err1 := os.OpenFile(r.config.outputFilePath, os.O_CREATE|os.O_WRONLY, 0644)
		if err1 != nil {
			log.Printf("unable to open unencrypted volumes file: %v", err1)
			return
		}

		defer func() {
			if err2 := contents.Close(); err2 != nil {
				log.Printf("Unable to close unencrypted volumes file: %v", err2)
			}
		}()

		printable, err1 := json.MarshalIndent(unencryptedVolumes, "", "  ")
		if err1 != nil {
			log.Printf("Unable to marshal unencrypted volumes: %v", err1)
			return
		}
		if string(printable) == "null" {
			log.Printf("Unencrypted volumes list is empty\n")
			return
		}

		//log.Printf("Unencrypted Volumes:\n")
		//log.Println(string(printable))

		if err1 = contents.Truncate(0); err1 != nil {
			log.Printf("unable to truncate unencrypted volumes file: %v", err1)
			return
		}

		if _, err1 = contents.WriteString(string(printable)); err1 != nil {
			log.Printf("unable to write unencrypted volumes list to file: %v", err1)
		}
	}()

	for _, etcd := range etcds {
		if etcd.Spec.Backup.Store == nil {
			continue
		}
		provider, err := utils.StorageProviderFromInfraProvider(etcd.Spec.Backup.Store.Provider)
		if err != nil {
			return fmt.Errorf("unable to get storage provider for etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
		}
		if provider != utils.S3 {
			log.Printf("Skipping etcd %s/%s with storage provider %s\n", etcd.Namespace, etcd.Name, provider)
			continue
		}

		log.Println("--------------------")
		log.Printf("etcd: %s, replicas: %d\n", r.getEtcdFullName(&etcd), etcd.Spec.Replicas)

		if !strings.Contains(etcd.Namespace, "shoot--") {
			continue
		}
		// TODO: remove
		//if slices.Contains([]string{"shoot--i030268--ash27102023", "shoot--i544024--local-seed-1"}, etcd.Namespace) {
		//	continue
		//}

		// TODO: remove
		//if !strings.Contains(etcd.Namespace, "shoot--i349079--aws") {
		//	continue
		//}
		//if etcd.Namespace != "shoot--i349079--aws-01" {
		//	continue
		//}

		// set AWS_REGION env var for taking full snapshot
		etcdBackupSecret := &corev1.Secret{}
		if err = c.Get(context.Background(), client.ObjectKey{Name: "etcd-backup", Namespace: etcd.Namespace}, etcdBackupSecret); err != nil {
			return fmt.Errorf("unable to get etcd backup secret for etcd %s: %v", r.getEtcdFullName(&etcd), err)
		}
		region, ok := etcdBackupSecret.Data["region"]
		if !ok {
			return fmt.Errorf("region not found in etcd backup secret for etcd %s", r.getEtcdFullName(&etcd))
		}
		if err = os.Setenv(AWS_REGION, string(region)); err != nil {
			return fmt.Errorf("unable to set %s env var: %v", AWS_REGION, err)
		}

		// TODO: pass stsLabels and pvcs to the handler functions to avoid second set of calls to the same functions
		log.Printf("Fetching pvc count for etcd %s/%s\n", etcd.Namespace, etcd.Name)
		stsLabels, err := getEtcdStatefulSetLabels(c, &etcd)
		if err != nil {
			return fmt.Errorf("unable to get statefulset labels for etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
		}
		pvcs, err := getPVCsForEtcd(c, &etcd, stsLabels)
		if err != nil {
			return fmt.Errorf("unable to get pvc count for etcd %s/%s: %v", etcd.Namespace, etcd.Name, err)
		}
		pvcCount := len(pvcs)
		if pvcCount == 0 {
			log.Printf("etcd %s/%s has no PVCs", etcd.Namespace, etcd.Name)
			continue
		} else if pvcCount%2 == 0 {
			return fmt.Errorf("etcd %s/%s has even number of PVCs: %d", etcd.Namespace, etcd.Name, pvcCount)
		}
		log.Printf("etcd %s/%s has %d PVCs\n", etcd.Namespace, etcd.Name, pvcCount)

		if isSingleNodeEtcd(&etcd, pvcCount) {
			log.Printf("Handling single-node etcd %s\n", r.getEtcdFullName(&etcd))
			if err = r.handleSingleNode(c, clientSet, config, &etcd, pvNodeMap, &unencryptedVolumes); err != nil {
				return fmt.Errorf("handling of single-node etcd %s/%s failed with error: %v", etcd.Namespace, etcd.Name, err)
			}
			log.Printf("Handled single-node etcd %s\n", r.getEtcdFullName(&etcd))
		} else if isMultiNodeEtcd(&etcd, pvcCount) {
			// TODO: remove
			log.Printf("Will not handle multi-node etcd %s\n", r.getEtcdFullName(&etcd))
			continue

			log.Printf("Handling multi-node etcd %s\n", r.getEtcdFullName(&etcd))
			if err = r.handleMultiNode(c, clientSet, config, &etcd, pvNodeMap, &unencryptedVolumes); err != nil {
				return fmt.Errorf("handling of multi-node etcd %s/%s failed with error: %v", etcd.Namespace, etcd.Name, err)
			}
			log.Printf("Handled multi-node etcd %s\n", r.getEtcdFullName(&etcd))
		} else {
			return fmt.Errorf("etcd %s is neither single-node nor multi-node", r.getEtcdFullName(&etcd))
		}
	}

	return nil
}

func printHelp() {
	log.Printf(`
The goal of this script is to encrypt all unencrypted AWS volumes that are backing etcd-main pods.

Usage:
    %[1]s: (--kubeconfig </path/to/the/kubeconfig> [--dry-run])

Options:
    --kubeconfig: Path to kubeconfig file. (mandatory).
    --dry-run[=true]: Indicates whether to perform volume deletions or simply print etcd information (optional) -> true or false.
    --help: Prints this help doc.

Examples:
    1.  %[1]s --kubeconfig ~/kuebconfig.yaml # Prints all etcd-main pods and associated AWS EBS volumes eligible for encryption.
    2.  %[1]s --kubeconfig ~/kuebconfig.yaml --dry-run=false # Replaces all unencrypted AWS EBS volumes backing etcd-main pods.
`, os.Args[0])
}

func (r *runner) summarize() error {
	contents, err := os.ReadFile(r.config.outputFilePath)
	if err != nil {
		return fmt.Errorf("unable to read unencrypted volumes file: %v", err)
	}
	var volumes []*volume
	if err = json.Unmarshal(contents, &volumes); err != nil {
		return fmt.Errorf("unable to unmarshal unencrypted volumes file: %v", err)
	}

	var (
		totalVolumes        = 0
		clustersVolumeCount = make(map[string]int)
		regionsVolumeCount  = make(map[string]int)
		singleNodeCount     = 0
		multiNodeCount      = 0

		totalUnencryptedVolumes        = 0
		clustersUnencryptedVolumeCount = make(map[string]int)
		regionsUnencryptedVolumeCount  = make(map[string]int)
		singleNodeUnencryptedCount     = 0
		multiNodeUnencryptedCount      = 0
	)

	for _, v := range volumes {
		totalVolumes++

		if _, ok := clustersVolumeCount[v.Cluster]; !ok {
			clustersVolumeCount[v.Cluster] = 0
		}
		clustersVolumeCount[v.Cluster]++

		if _, ok := regionsVolumeCount[v.Region]; !ok {
			regionsVolumeCount[v.Region] = 0
		}
		regionsVolumeCount[v.Region]++

		if v.EtcdReplicas == 1 {
			singleNodeCount++
		} else if v.EtcdReplicas == 3 {
			multiNodeCount++
		}

		if !v.Encrypted {
			totalUnencryptedVolumes++

			if _, ok := clustersUnencryptedVolumeCount[v.Cluster]; !ok {
				clustersUnencryptedVolumeCount[v.Cluster] = 0
			}
			clustersUnencryptedVolumeCount[v.Cluster]++

			if _, ok := regionsUnencryptedVolumeCount[v.Region]; !ok {
				regionsUnencryptedVolumeCount[v.Region] = 0
			}
			regionsUnencryptedVolumeCount[v.Region]++

			if v.EtcdReplicas == 1 {
				singleNodeUnencryptedCount++
			} else if v.EtcdReplicas == 3 {
				multiNodeUnencryptedCount++
			}
		}
	}

	clustersVolumeCountPrintable, err := json.MarshalIndent(clustersVolumeCount, "", "  ")
	if err != nil {
		return fmt.Errorf("unable to marshal seeds volume count: %v", err)
	}
	regionsVolumeCountPrintable, err := json.MarshalIndent(regionsVolumeCount, "", "  ")
	if err != nil {
		return fmt.Errorf("unable to marshal regions volume count: %v", err)
	}

	clustersUnencryptedVolumeCountPrintable, err := json.MarshalIndent(clustersUnencryptedVolumeCount, "", "  ")
	if err != nil {
		return fmt.Errorf("unable to marshal seeds unencrypted volume count: %v", err)
	}
	regionsUnencryptedVolumeCountPrintable, err := json.MarshalIndent(regionsUnencryptedVolumeCount, "", "  ")
	if err != nil {
		return fmt.Errorf("unable to marshal regions unencrypted volume count: %v", err)
	}

	log.Printf("Summary:\nTotal volumes: %d\nVolumes per seed:\n%s\nVolumes per region:\n%s\nVolumes for single-node etcds: %d\nVolumes for multi-node etcds: %d\n", totalVolumes, clustersVolumeCountPrintable, regionsVolumeCountPrintable, singleNodeCount, multiNodeCount)
	log.Printf("Total unencrypted volumes: %d\nUnencrypted volumes per seed:\n%s\nUnencrypted volumes per region:\n%s\nUnencrypted volumes for single-node etcds: %d\nUnencrypted volumes for multi-node etcds: %d\n", totalUnencryptedVolumes, clustersUnencryptedVolumeCountPrintable, regionsUnencryptedVolumeCountPrintable, singleNodeUnencryptedCount, multiNodeUnencryptedCount)
	return nil
}

func main() {
	var help bool

	flag.CommandLine = flag.NewFlagSet(os.Args[0], flag.ExitOnError)

	flag.BoolVar(&help, "help", false, "Prints the help doc.")

	r := runner{}
	r.addFlags()

	flag.Parse()

	if help {
		printHelp()
		os.Exit(0)
	}

	if err := r.validateConfig(); err != nil {
		log.Printf("Invalid config: %v\n", err)
		printHelp()
		os.Exit(1)
	}

	log.Printf("Running with runner config %+v", r.config)

	if r.config.summarize {
		if err := r.summarize(); err != nil {
			log.Fatalf("Summarization failed with error: %v", err)
		}
		os.Exit(0)
	}

	if err := r.run(); err != nil {
		log.Fatalf("Runner failed with error: %v", err)
	}
}

// TODO: take output json file flag and print out unencrypted volume info (pvc name, ns, seed, region, volume id) to the file
// TODO: write shell script to get aws seeds and run this script per seed, first in dry-run mode to get unencrypted volumes, then in non-dry-run mode to replace them. Pass output dir, so that script can pass file name flag to this script.
