// Copyright (c) 2022 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package etcdmember

import (
	"context"
	"fmt"

	druidv1alpha1 "github.com/gardener/etcd-druid/api/v1alpha1"
	"github.com/gardener/gardener/pkg/controllerutils"
	gardenercomponent "github.com/gardener/gardener/pkg/operation/botanist/component"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type component struct {
	client    client.Client
	namespace string

	values Values
}

func (c *component) Deploy(ctx context.Context) error {
	var (
		etcdMembers []*druidv1alpha1.EtcdMember
	)
	for _, memberName := range c.values.Names {
		etcdMembers = append(etcdMembers, c.emptyEtcdMember(memberName))
	}

	if err := c.syncEtcdMembers(ctx, etcdMembers); err != nil {
		return err
	}

	return nil
}

func (c *component) Destroy(ctx context.Context) error {
	var (
		etcdMembers []*druidv1alpha1.EtcdMember
	)
	for _, memberName := range c.values.Names {
		etcdMembers = append(etcdMembers, c.emptyEtcdMember(memberName))
	}

	if err := c.deleteEtcdMembers(ctx, etcdMembers); err != nil {
		return err
	}

	return nil
}

// New creates a new etcdmember deployer instance.
func New(c client.Client, namespace string, values Values) gardenercomponent.Deployer {
	return &component{
		client:    c,
		namespace: namespace,
		values:    values,
	}
}

func (c *component) emptyEtcdMember(name string) *druidv1alpha1.EtcdMember {
	return &druidv1alpha1.EtcdMember{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: c.namespace,
		},
	}
}

func getOwnerReferences(val Values) []metav1.OwnerReference {
	return []metav1.OwnerReference{
		{
			APIVersion:         druidv1alpha1.GroupVersion.String(),
			Kind:               "Etcd",
			Name:               val.EtcdName,
			UID:                val.EtcdUID,
			Controller:         pointer.BoolPtr(true),
			BlockOwnerDeletion: pointer.BoolPtr(true),
		},
	}
}

func getLabels(val Values) map[string]string {
	labels := map[string]string{
		"instance": val.EtcdName,
	}

	for k, v := range val.Labels {
		labels[k] = v
	}

	return labels
}

func (c *component) syncEtcdMembers(ctx context.Context, etcdMembers []*druidv1alpha1.EtcdMember) error {
	var errList []error

	for i, etcdMember := range etcdMembers {
		if _, err := controllerutils.GetAndCreateOrMergePatch(ctx, c.client, etcdMember, func() error {
			etcdMember.Labels = getLabels(c.values)
			etcdMember.OwnerReferences = getOwnerReferences(c.values)
			etcdMember.Spec.PeerTLSEnabled = &c.values.PeerTLSEnabled
			etcdMember.Spec.PVCRef = &druidv1alpha1.NamespacedObjectReference{
				Name:      c.values.PVCNames[i],
				Namespace: c.namespace,
			}
			return nil
		}); err != nil {
			errList = append(errList, err)
		}
	}

	if len(errList) != 0 {
		var errs error
		for _, err := range errList {
			errs = fmt.Errorf("%v; %v", errs, err)
		}
		return errs
	}
	return nil
}

func (c *component) deleteEtcdMembers(ctx context.Context, etcdMembers []*druidv1alpha1.EtcdMember) error {
	var errList []error

	for _, etcdMember := range etcdMembers {
		if err := client.IgnoreNotFound(c.client.Delete(ctx, etcdMember)); err != nil {
			errList = append(errList, err)
		}
	}

	if len(errList) != 0 {
		var errs error
		for _, err := range errList {
			errs = fmt.Errorf("%v; %v", errs, err)
		}
		return errs
	}
	return nil
}
