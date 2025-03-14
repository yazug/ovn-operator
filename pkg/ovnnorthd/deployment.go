/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at
    http://www.apache.org/licenses/LICENSE-2.0
Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ovnnorthd

import (
	"fmt"

	"github.com/openstack-k8s-operators/lib-common/modules/common"
	"github.com/openstack-k8s-operators/lib-common/modules/common/affinity"
	"github.com/openstack-k8s-operators/lib-common/modules/common/env"
	ovnv1 "github.com/openstack-k8s-operators/ovn-operator/api/v1beta1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ServiceCommand -
	ServiceCommand = "/usr/bin/ovn-northd"
)

// Deployment func
func Deployment(
	instance *ovnv1.OVNNorthd,
	labels map[string]string,
	annotations map[string]string,
	nbEndpoint string,
	sbEndpoint string,
) *appsv1.Deployment {

	livenessProbe := &corev1.Probe{
		// TODO might need tuning
		TimeoutSeconds:      5,
		PeriodSeconds:       3,
		InitialDelaySeconds: 3,
	}
	readinessProbe := &corev1.Probe{
		// TODO might need tuning
		TimeoutSeconds:      5,
		PeriodSeconds:       5,
		InitialDelaySeconds: 5,
	}
	cmd := ServiceCommand
	args := []string{
		"-vfile:off",
		fmt.Sprintf("-vconsole:%s", instance.Spec.LogLevel),
		fmt.Sprintf("--ovnnb-db=%s", nbEndpoint),
		fmt.Sprintf("--ovnsb-db=%s", sbEndpoint),
	}

	if instance.Spec.Debug.Service {
		cmd = "/bin/sleep"
		args = []string{"infinity"}

		noopCmd := []string{
			"/bin/true",
		}
		livenessProbe.Exec = &corev1.ExecAction{
			Command: noopCmd,
		}

		readinessProbe.Exec = &corev1.ExecAction{
			Command: noopCmd,
		}
	} else {
		//
		// https://kubernetes.io/docs/tasks/configure-pod-container/configure-liveness-readiness-startup-probes/
		//
		livenessProbe.Exec = &corev1.ExecAction{
			Command: []string{
				"/usr/bin/pidof", "ovn-northd",
			},
		}
		readinessProbe.Exec = livenessProbe.Exec
	}

	envVars := map[string]env.Setter{}
	// TODO: Make confs customizable
	envVars["OVN_RUNDIR"] = env.SetValue("/tmp")

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ServiceName,
			Namespace: instance.Namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Replicas: instance.Spec.Replicas,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: annotations,
					Labels:      labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: instance.RbacResourceName(),
					Containers: []corev1.Container{
						{
							Name:                     ServiceName,
							Command:                  []string{cmd},
							Args:                     args,
							Image:                    instance.Spec.ContainerImage,
							SecurityContext:          getOVNNorthdSecurityContext(),
							Env:                      env.MergeEnvs([]corev1.EnvVar{}, envVars),
							Resources:                instance.Spec.Resources,
							ReadinessProbe:           readinessProbe,
							LivenessProbe:            livenessProbe,
							TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
						},
					},
				},
			},
		},
	}
	// If possible two pods of the same service should not
	// run on the same worker node. If this is not possible
	// the get still created on the same worker node.
	deployment.Spec.Template.Spec.Affinity = affinity.DistributePods(
		common.AppSelector,
		[]string{
			ServiceName,
		},
		corev1.LabelHostname,
	)
	if instance.Spec.NodeSelector != nil && len(instance.Spec.NodeSelector) > 0 {
		deployment.Spec.Template.Spec.NodeSelector = instance.Spec.NodeSelector
	}

	return deployment
}
