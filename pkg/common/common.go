// Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
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

package common

const (
	// Etcd is the key for the etcd image in image vector
	Etcd = "etcd"
	// BackupRestore is the key for the etcd-backup-restore image in image vector
	BackupRestore = "etcd-backup-restore"
	// ChartPath is the directory that contains the default image vector file
	ChartPath = "charts"
	// GardenerOwnedBy is a constant for an annotation on a resource that describes the owner resource.
	GardenerOwnedBy = "gardener.cloud/owned-by"
	// GardenerOwnerType is a constant for an annotation on a resource that describes the type of owner resource.
	GardenerOwnerType = "gardener.cloud/owner-type"
	// PVCDeletionConfirmationAnnotation is the annotation required on the Etcd resource
	// in order to allow druid to delete the PVC(s) used by the etcd pods
	PVCDeletionConfirmationAnnotation = "confirmation.druid.gardener.cloud/pvc-deletion"
)
