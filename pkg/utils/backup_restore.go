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

package utils

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"strings"
	"time"

	brtypes "github.com/gardener/etcd-backup-restore/pkg/types"
	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	BackupRestoreContainerName = "backup-restore"
)

// TODO: export from backup-restore, instead of redefining here
type latestSnapshotMetadataResponse struct {
	FullSnapshot   *brtypes.Snapshot `json:"fullSnapshot"`
	DeltaSnapshots brtypes.SnapList  `json:"deltaSnapshots"`
}

func getSnapshotCommandPath(podName string, tlsEnabled bool, backupRestorePort int32) string {
	httpScheme := "http"
	if tlsEnabled {
		httpScheme = "https"
	}

	tokens := strings.Split(podName, "-")
	etcdName := strings.Join(tokens[:len(tokens)-1], "-")

	return fmt.Sprintf("curl -k %s://%s-local:%s/snapshot", httpScheme, etcdName, string(backupRestorePort))
}

func getTriggerSnapshotCommand(podName string, tlsEnabled bool, backupRestorePort int32, snapshotType druidv1alpha1.SnapshotType) string {
	snapType := "full"
	if snapshotType == druidv1alpha1.SnapshotTypeIncr {
		snapType = "delta"
	}

	return fmt.Sprintf("%s/%s", getSnapshotCommandPath(podName, tlsEnabled, backupRestorePort), snapType)
}

func TriggerSnapshot(config *rest.Config, podName, podNamespace string, tlsEnabled bool, backupRestorePort int32, snapshotType druidv1alpha1.SnapshotType) error {
	podExecutor := kubernetes.NewPodExecutor(config)
	_, err := podExecutor.Execute(
		podNamespace,
		podName,
		BackupRestoreContainerName,
		"/bin/sh",
		getTriggerSnapshotCommand(podName, tlsEnabled, backupRestorePort, snapshotType),
	)
	return err
}

func getLatestSnapshotsCommand(podName string, tlsEnabled bool, backupRestorePort int32) string {
	return fmt.Sprintf("%s/%s", getSnapshotCommandPath(podName, tlsEnabled, backupRestorePort), "latest")
}

func getLatestSnapshots(config *rest.Config, podName, podNamespace string, tlsEnabled bool, backupRestorePort int32) (latestSnapshotMetadataResponse, error) {
	var (
		err             error
		resp            io.Reader
		latestSnapshots latestSnapshotMetadataResponse
	)

	podExecutor := kubernetes.NewPodExecutor(config)
	if resp, err = podExecutor.Execute(
		podNamespace,
		podName,
		BackupRestoreContainerName,
		"/bin/sh",
		getLatestSnapshotsCommand(podName, tlsEnabled, backupRestorePort),
	); err != nil {
		return latestSnapshotMetadataResponse{}, err
	}

	body, err := ioutil.ReadAll(resp)
	if err != nil {
		return latestSnapshotMetadataResponse{}, err
	}

	if err = json.Unmarshal(body, &latestSnapshots); err != nil {
		return latestSnapshotMetadataResponse{}, err
	}

	return latestSnapshots, nil
}

func GetLatestSnapshotTimestamp(config *rest.Config, podName, podNamespace string, tlsEnabled bool, backupRestorePort int32) (*time.Time, error) {
	var (
		snapshots latestSnapshotMetadataResponse
		err       error
	)
	if snapshots, err = getLatestSnapshots(config, podName, podNamespace, tlsEnabled, backupRestorePort); err != nil {
		return nil, err
	}

	latestSnapshot := snapshots.FullSnapshot
	for _, snapshot := range snapshots.DeltaSnapshots {
		if snapshot.CreatedOn.After(latestSnapshot.CreatedOn) {
			latestSnapshot = snapshot
		}
	}

	return &latestSnapshot.CreatedOn, nil
}
