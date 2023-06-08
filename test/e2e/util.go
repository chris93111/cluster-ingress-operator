//go:build e2e
// +build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	configv1 "github.com/openshift/api/config/v1"
	operatorv1 "github.com/openshift/api/operator/v1"
	routev1 "github.com/openshift/api/route/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/pointer"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// buildEchoPod returns a pod definition for an socat-based echo server.
func buildEchoPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app": name,
			},
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					// Note that HTTP/1.0 will strip the HSTS response header
					Args: []string{
						"TCP4-LISTEN:8080,reuseaddr,fork",
						`EXEC:'/bin/bash -c \"printf \\\"HTTP/1.0 200 OK\r\n\r\n\\\"; sed -e \\\"/^\r/q\\\"\"'`,
					},
					Command: []string{"/bin/socat"},
					Image:   "openshift/origin-node",
					Name:    "echo",
					Ports: []corev1.ContainerPort{
						{
							ContainerPort: int32(8080),
							Protocol:      corev1.ProtocolTCP,
						},
					},
				},
			},
		},
	}
}

// generateUnprivilegedSecurityContext returns a SecurityContext with the minimum possible privileges that satisfy
// restricted pod security requirements
func generateUnprivilegedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: pointer.Bool(false),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
		RunAsNonRoot: pointer.Bool(true),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}

// buildEchoService returns a service definition for an HTTP service.
func buildEchoService(name, namespace string, labels map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       int32(80),
					Protocol:   corev1.ProtocolTCP,
					TargetPort: intstr.FromInt(8080),
				},
			},
			Selector: labels,
		},
	}
}

// buildCurlPod returns a pod definition for a pod with the given name and image
// and in the given namespace that curls the specified host and address.
func buildCurlPod(name, namespace, image, host, address string, extraArgs ...string) *corev1.Pod {
	curlArgs := []string{
		"-s",
		"--retry", "300", "--retry-delay", "1", "--max-time", "2",
		"--resolve", host + ":80:" + address,
	}
	curlArgs = append(curlArgs, extraArgs...)
	curlArgs = append(curlArgs, "http://"+host)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "curl",
					Image:   image,
					Command: []string{"/bin/curl"},
					Args:    curlArgs,
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
}

// buildExecPod returns a pod definition for a pod with the given name and image
// and in the given namespace that sleeps for 4 hours (or until it is deleted),
// which can be used to exec commands inside the pod.
func buildExecPod(name, namespace, image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "execpod",
					Image:   image,
					Command: []string{"/bin/sleep"},
					Args:    []string{"4h"},
				},
			},
			RestartPolicy: corev1.RestartPolicyNever,
		},
	}
}

// buildSlowHTTPDPod returns a pod that responds to HTTP requests slowly.
func buildSlowHTTPDPod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Labels: map[string]string{
				"app": "slow-httpd",
			},
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Args: []string{
						"TCP4-LISTEN:8080,reuseaddr,fork",
						`EXEC:'/bin/bash -c \"sleep 40; printf \\\"HTTP/1.0 200 OK\r\n\r\nfin\r\n\\\"\"'`,
					},
					Command: []string{"/bin/socat"},
					Image:   "openshift/origin-node",
					Name:    "echo",
					Ports: []corev1.ContainerPort{
						{
							ContainerPort: int32(8080),
							Protocol:      corev1.ProtocolTCP,
						},
					},
				},
			},
		},
	}
}

// buildRoute returns a route definition targeting the specified service.
func buildRoute(name, namespace, serviceName string) *routev1.Route {
	return &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: routev1.RouteSpec{
			To: routev1.RouteTargetReference{
				Kind: "Service",
				Name: serviceName,
			},
		},
	}
}

// buildRouteWithHSTS returns a route definition with the specified HSTS annotation.
// Overwrites Spec.Host and TLS
func buildRouteWithHSTS(podName, namespace, serviceName, domain, annotation string) *routev1.Route {
	route := buildRoute(podName, namespace, serviceName)
	route.Spec.Host = domain
	route.Spec.TLS = &routev1.TLSConfig{Termination: routev1.TLSTerminationEdge}
	if route.Annotations == nil {
		route.Annotations = map[string]string{}
	}
	route.Annotations["haproxy.router.openshift.io/hsts_header"] = annotation

	return route
}

func getIngressController(t *testing.T, client client.Client, name types.NamespacedName, timeout time.Duration) (*operatorv1.IngressController, error) {
	t.Helper()
	ic := operatorv1.IngressController{}
	if err := wait.PollImmediate(1*time.Second, timeout, func() (bool, error) {
		if err := client.Get(context.TODO(), name, &ic); err != nil {
			t.Logf("Get %q failed: %v, retrying...", name, err)
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("failed to get %q: %v", name, err)
	}
	return &ic, nil
}

func getDeployment(t *testing.T, client client.Client, name types.NamespacedName, timeout time.Duration) (*appsv1.Deployment, error) {
	t.Helper()
	dep := appsv1.Deployment{}
	if err := wait.PollImmediate(1*time.Second, timeout, func() (bool, error) {
		if err := client.Get(context.TODO(), name, &dep); err != nil {
			t.Logf("Get %q failed: %v, retrying...", name, err)
			return false, nil
		}
		return true, nil
	}); err != nil {
		return nil, fmt.Errorf("Failed to get %q: %v", name, err)
	}
	return &dep, nil
}

func podExec(t *testing.T, pod corev1.Pod, stdout, stderr *bytes.Buffer, cmd []string) error {
	t.Helper()
	kubeConfig, err := config.GetConfig()
	if err != nil {
		t.Fatalf("failed to get kube config: %v", err)
	}
	cl, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		t.Fatalf("failed to create kube client: %v", err)
	}
	req := cl.CoreV1().RESTClient().Post().Resource("pods").
		Namespace(pod.Namespace).Name(pod.Name).SubResource("exec").
		Param("container", pod.Spec.Containers[0].Name).
		VersionedParams(&corev1.PodExecOptions{
			Command: cmd,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(kubeConfig, "POST", req.URL())
	if err != nil {
		return err
	}
	return exec.Stream(remotecommand.StreamOptions{
		Stdout: stdout,
		Stderr: stderr,
	})
}

// cmpProbes compares two probes on their timeoutSeconds, periodSeconds,
// successThreshold, and failureThreshold parameters.
func cmpProbes(a, b *corev1.Probe) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.TimeoutSeconds != b.TimeoutSeconds {
		return false
	}
	if a.PeriodSeconds != b.PeriodSeconds {
		return false
	}
	if a.SuccessThreshold != b.SuccessThreshold {
		return false
	}
	if a.FailureThreshold != b.FailureThreshold {
		return false
	}
	return true
}

// probe returns a Probe with the specified parameters.
func probe(timeout, period, success, failure int) *corev1.Probe {
	return &corev1.Probe{
		TimeoutSeconds:   int32(timeout),
		PeriodSeconds:    int32(period),
		SuccessThreshold: int32(success),
		FailureThreshold: int32(failure),
	}
}

// updateIngressControllerSpecWithRetryOnConflict gets a fresh copy of
// the named ingresscontroller, calls mutateSpecFn() where callers can
// modify fields of the spec, and then updates the ingresscontroller
// object. If there is a conflict error on update then the complete
// sequence of get, mutate, and update is retried until timeout is
// reached.
func updateIngressControllerSpecWithRetryOnConflict(t *testing.T, name types.NamespacedName, timeout time.Duration, mutateSpecFn func(*operatorv1.IngressControllerSpec)) error {
	ic := operatorv1.IngressController{}
	return wait.PollImmediate(1*time.Second, timeout, func() (bool, error) {
		if err := kclient.Get(context.TODO(), name, &ic); err != nil {
			t.Logf("error getting ingress controller %v: %v, retrying...", name, err)
			return false, nil
		}
		mutateSpecFn(&ic.Spec)
		if err := kclient.Update(context.TODO(), &ic); err != nil {
			if errors.IsConflict(err) {
				t.Logf("conflict when updating ingress controller %v: %v, retrying...", name, err)
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

// updateIngressConfigSpecWithRetryOnConflict gets a fresh copy of the
// name ingress config, calls updateSpecFn() where callers can modify
// fields of the spec, and then updates the ingress config object. If
// there is a conflict error on update then the complete operation of
// get, mutate, and update is retried until timeout is reached.
func updateIngressConfigSpecWithRetryOnConflict(t *testing.T, name types.NamespacedName, timeout time.Duration, mutateSpecFn func(*configv1.IngressSpec)) error {
	ingressConfig := configv1.Ingress{}
	return wait.PollImmediate(1*time.Second, timeout, func() (bool, error) {
		if err := kclient.Get(context.TODO(), name, &ingressConfig); err != nil {
			t.Logf("error getting ingress config %v: %v, retrying...", name, err)
			return false, nil
		}
		mutateSpecFn(&ingressConfig.Spec)
		if err := kclient.Update(context.TODO(), &ingressConfig); err != nil {
			if errors.IsConflict(err) {
				t.Logf("conflict when updating ingress config %v: %v, retrying...", name, err)
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

// updateInfrastructureConfigSpecWithRetryOnConflict gets a fresh copy
// of the named infrastructure object, calls updateSpecFn() where
// callers can modify fields of the spec, and then updates the infrastructure
// config object. If there is a conflict error on update then the
// complete operation of get, mutate, and update is retried until
// timeout is reached.
func updateInfrastructureConfigSpecWithRetryOnConflict(t *testing.T, name types.NamespacedName, timeout time.Duration, mutateSpecFn func(*configv1.InfrastructureSpec)) error {
	infraConfig := configv1.Infrastructure{}
	return wait.PollImmediate(1*time.Second, timeout, func() (bool, error) {
		if err := kclient.Get(context.TODO(), name, &infraConfig); err != nil {
			t.Logf("error getting infrastructure config %v: %v, retrying...", name, err)
			return false, nil
		}
		mutateSpecFn(&infraConfig.Spec)
		if err := kclient.Update(context.TODO(), &infraConfig); err != nil {
			if errors.IsConflict(err) {
				t.Logf("conflict when updating infrastructure config %v: %v, retrying...", name, err)
				return false, nil
			}
			return false, err
		}
		return true, nil
	})
}

// assertDeleted tries to delete a cluster resource, and causes test failure if the delete fails.
func assertDeleted(t *testing.T, cl client.Client, thing client.Object) {
	t.Helper()
	if err := cl.Delete(context.TODO(), thing); err != nil {
		if errors.IsNotFound(err) {
			return
		}
		t.Fatalf("Failed to delete %s: %v", thing.GetName(), err)
	} else {
		t.Logf("Deleted %s", thing.GetName())
	}
}

// assertDeletedWaitForCleanup tries to delete a cluster resource, and waits for it to actually be cleaned up before
// returning. It causes test failure if the delete fails or if the cleanup times out.
func assertDeletedWaitForCleanup(t *testing.T, cl client.Client, thing client.Object) {
	t.Helper()
	thingName := types.NamespacedName{
		Name:      thing.GetName(),
		Namespace: thing.GetNamespace(),
	}
	assertDeleted(t, cl, thing)
	if err := wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		if err := cl.Get(context.TODO(), thingName, thing); err != nil {
			if errors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		return false, nil
	}); err != nil {
		t.Fatalf("Timed out waiting for %s to be cleaned up: %v", thing.GetName(), err)
	}
}
