package project_cluster

import (
	"context"
	"fmt"

	apisv3 "github.com/rancher/rancher/pkg/apis/management.cattle.io/v3"
	"github.com/rancher/rancher/pkg/rbac"
	corev1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	crbacv1 "github.com/rancher/wrangler/v3/pkg/generated/controllers/rbac/v1"
	"github.com/rancher/wrangler/v3/pkg/name"
	"github.com/sirupsen/logrus"
	v12 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	v1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	CreatorIDAnnotation             = "field.cattle.io/creatorId"
	NoCreatorRBACAnnotation         = "field.cattle.io/no-creator-rbac"
	creatorPrincipalNameAnnotation  = "field.cattle.io/creator-principal-name"
	creatorOwnerBindingAnnotation   = "authz.management.cattle.io/creator-owner-binding"
	roleTemplatesRequiredAnnotation = "authz.management.cattle.io/creator-role-bindings"
	clusterNameLabel                = "cluster.cattle.io/name"
	projectContext                  = "project"
	clusterContext                  = "cluster"
)

var crtbCreatorOwnerAnnotations = map[string]string{creatorOwnerBindingAnnotation: "true"}

func deleteNamespace(controller string, nsName string, nsClient v1.NamespaceInterface) error {
	ns, err := nsClient.Get(context.TODO(), nsName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if ns.Status.Phase != v12.NamespaceTerminating {
		logrus.Infof("[%s] Deleting namespace %s", controller, nsName)
		err = nsClient.Delete(context.TODO(), nsName, metav1.DeleteOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
	}
	return err
}

func reconcileResourceToNamespace(obj runtime.Object, controller string, nsName string, nsLister corev1.NamespaceCache, nsClient v1.NamespaceInterface) (runtime.Object, error) {
	return apisv3.NamespaceBackedResource.Do(obj, func() (runtime.Object, error) {
		t, err := meta.TypeAccessor(obj)
		if err != nil {
			return obj, err
		}

		ns, _ := nsLister.Get(nsName)
		if ns == nil {
			logrus.Infof("[%v] Creating namespace %v", controller, nsName)
			_, err := nsClient.Create(context.TODO(), &v12.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: nsName,
					Annotations: map[string]string{
						"management.cattle.io/system-namespace": "true",
					},
				},
			}, metav1.CreateOptions{})
			if err != nil {
				return obj, fmt.Errorf("[%s] failed to create namespace for %s %s: %w", controller, t.GetKind(), nsName, err)
			}
		}

		return obj, nil
	})
}

// createMembershipRoles creates 2 cluster roles: an owner role and a member role. To be used to create project/cluster membership roles.
func createMembershipRoles(obj runtime.Object, crClient crbacv1.ClusterRoleController) error {
	var resourceName, resourceType, context string
	var annotations map[string]string

	switch v := obj.(type) {
	case *apisv3.Project:
		context = projectContext
		resourceType = apisv3.ProjectResourceName
		resourceName = v.GetName()
	case *apisv3.Cluster:
		annotations = map[string]string{clusterNameLabel: v.GetName()}
		context = clusterContext
		resourceType = apisv3.ClusterResourceName
		resourceName = v.GetName()
	default:
		return fmt.Errorf("cannot create membership roles for unsupported type %T", v)
	}

	ownerRef, err := newOwnerReference(obj)
	if err != nil {
		return err
	}

	memberRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name.SafeConcatName(resourceName, context, "member"),
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Annotations:     annotations,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{apisv3.SchemeGroupVersion.Group},
				Resources:     []string{resourceType},
				ResourceNames: []string{resourceName},
				Verbs:         []string{"get"},
			},
		},
	}

	ownerRole := &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name.SafeConcatName(resourceName, context, "owner"),
			OwnerReferences: []metav1.OwnerReference{ownerRef},
			Annotations:     annotations,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups:     []string{apisv3.SchemeGroupVersion.Group},
				Resources:     []string{resourceType},
				ResourceNames: []string{resourceName},
				Verbs:         []string{rbacv1.VerbAll},
			},
		},
	}

	for _, cr := range []*rbacv1.ClusterRole{memberRole, ownerRole} {
		if err := rbac.CreateOrUpdateResource(cr, crClient, rbac.AreClusterRolesSame); err != nil {
			return err
		}
	}

	return nil
}

// newOwnerReference create an OwnerReference from a runtime.Object.
func newOwnerReference(obj runtime.Object) (metav1.OwnerReference, error) {
	metadata, err := meta.Accessor(obj)
	if err != nil {
		return metav1.OwnerReference{}, fmt.Errorf("error accessing object %v: %w", obj, err)
	}
	typeInfo, err := meta.TypeAccessor(obj)
	if err != nil {
		return metav1.OwnerReference{}, fmt.Errorf("error accessing object %v type %T: %w", obj, obj, err)
	}
	return metav1.OwnerReference{
		APIVersion: typeInfo.GetAPIVersion(),
		Kind:       typeInfo.GetKind(),
		Name:       metadata.GetName(),
		UID:        metadata.GetUID(),
	}, nil
}
