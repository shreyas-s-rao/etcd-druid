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

package etcdmember

import (
	"fmt"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
)

const (
	defaultPeerTLSEnabled = false
)

// GenerateValues generates `etcdmember.Values` for the etcdmember component with the given `etcd` object.
func GenerateValues(etcd *druidv1alpha1.Etcd) Values {
	peerTLSEnabled := false
	if etcd.Spec.Etcd.PeerUrlTLS != nil {
		peerTLSEnabled = true
	}

	volumeClaimTemplateName := etcd.Name
	if etcd.Spec.VolumeClaimTemplate != nil && len(*etcd.Spec.VolumeClaimTemplate) != 0 {
		volumeClaimTemplateName = *etcd.Spec.VolumeClaimTemplate
	}

	var pvcNames, names []string
	for i := 0; i < int(etcd.Spec.Replicas); i++ {
		memberName := fmt.Sprintf("%s-%d", etcd.Name, i)
		pvcName := fmt.Sprintf("%s-%s", volumeClaimTemplateName, memberName)

		names = append(names, memberName)
		pvcNames = append(pvcNames, pvcName)
	}

	return Values{
		Names:          names,
		PeerTLSEnabled: peerTLSEnabled,
		PVCNames:       pvcNames,
		EtcdName:       etcd.Name,
		EtcdUID:        etcd.UID,
		Labels:         etcd.Spec.Labels,
	}
}
