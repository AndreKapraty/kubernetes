/*
Copyright 2014 The Kubernetes Authors.

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

package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/klog/v2"
	"k8s.io/kubernetes/pkg/features"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	kubetypes "k8s.io/kubernetes/pkg/kubelet/types"
	"k8s.io/kubernetes/pkg/kubelet/util/format"
	httpprobe "k8s.io/kubernetes/pkg/probe/http"
	"k8s.io/kubernetes/pkg/security/apparmor"
)

const (
	maxRespBodyLength = 10 * 1 << 10 // 10KB
)

type handlerRunner struct {
	httpDoer         kubetypes.HTTPDoer
	commandRunner    kubecontainer.CommandRunner
	containerManager podStatusProvider
}

type podStatusProvider interface {
	GetPodStatus(uid types.UID, name, namespace string) (*kubecontainer.PodStatus, error)
}

// NewHandlerRunner returns a configured lifecycle handler for a container.
func NewHandlerRunner(httpDoer kubetypes.HTTPDoer, commandRunner kubecontainer.CommandRunner, containerManager podStatusProvider) kubecontainer.HandlerRunner {
	return &handlerRunner{
		httpDoer:         httpDoer,
		commandRunner:    commandRunner,
		containerManager: containerManager,
	}
}

func (hr *handlerRunner) Run(containerID kubecontainer.ContainerID, pod *v1.Pod, container *v1.Container, handler *v1.LifecycleHandler) (string, error) {
	switch {
	case handler.Exec != nil:
		var msg string
		// TODO(tallclair): Pass a proper timeout value.
		output, err := hr.commandRunner.RunInContainer(containerID, handler.Exec.Command, 0)
		if err != nil {
			msg = fmt.Sprintf("Exec lifecycle hook (%v) for Container %q in Pod %q failed - error: %v, message: %q", handler.Exec.Command, container.Name, format.Pod(pod), err, string(output))
			klog.V(1).ErrorS(err, "Exec lifecycle hook for Container in Pod failed", "execCommand", handler.Exec.Command, "containerName", container.Name, "pod", klog.KObj(pod), "message", string(output))
		}
		return msg, err
	case handler.HTTPGet != nil:
		err := hr.runHTTPHandler(pod, container, handler)
		var msg string
		if err != nil {
			msg = fmt.Sprintf("HTTP lifecycle hook (%s) for Container %q in Pod %q failed - error: %v", handler.HTTPGet.Path, container.Name, format.Pod(pod), err)
			klog.V(1).ErrorS(err, "HTTP lifecycle hook for Container in Pod failed", "path", handler.HTTPGet.Path, "containerName", container.Name, "pod", klog.KObj(pod))
		}
		return msg, err
	default:
		err := fmt.Errorf("invalid handler: %v", handler)
		msg := fmt.Sprintf("Cannot run handler: %v", err)
		klog.ErrorS(err, "Cannot run handler")
		return msg, err
	}
}

// resolvePort attempts to turn an IntOrString port reference into a concrete port number.
// If portReference has an int value, it is treated as a literal, and simply returns that value.
// If portReference is a string, an attempt is first made to parse it as an integer.  If that fails,
// an attempt is made to find a port with the same name in the container spec.
// If a port with the same name is found, it's ContainerPort value is returned.  If no matching
// port is found, an error is returned.
func resolvePort(portReference intstr.IntOrString, container *v1.Container) (int, error) {
	if portReference.Type == intstr.Int {
		return portReference.IntValue(), nil
	}
	portName := portReference.StrVal
	port, err := strconv.Atoi(portName)
	if err == nil {
		return port, nil
	}
	for _, portSpec := range container.Ports {
		if portSpec.Name == portName {
			return int(portSpec.ContainerPort), nil
		}
	}
	return -1, fmt.Errorf("couldn't find port: %v in %v", portReference, container)
}

func (hr *handlerRunner) runHTTPHandler(pod *v1.Pod, container *v1.Container, handler *v1.LifecycleHandler) error {
	host := handler.HTTPGet.Host
	podIP := host
	if len(host) == 0 {
		status, err := hr.containerManager.GetPodStatus(pod.UID, pod.Name, pod.Namespace)
		if err != nil {
			klog.ErrorS(err, "Unable to get pod info, event handlers may be invalid.", "pod", klog.KObj(pod))
			return err
		}
		if len(status.IPs) == 0 {
			return fmt.Errorf("failed to find networking container: %v", status)
		}
		host = status.IPs[0]
		podIP = host
	}

	if utilfeature.DefaultFeatureGate.Enabled(features.ConsistentHTTPGetHandlers) {
		req, err := httpprobe.NewRequestForHTTPGetAction(handler.HTTPGet, container, podIP, "lifecycle")
		if err != nil {
			return err
		}
		resp, err := hr.httpDoer.Do(req)
		discardHTTPRespBody(resp)

		if isHTTPResponseError(err) {
			// TODO: emit an event about the fallback
			// TODO: increment a metric about the fallback
			klog.V(1).ErrorS(err, "HTTPS request to lifecycle hook got HTTP response, retrying with HTTP.", "pod", klog.KObj(pod), "host", req.URL.Host)

			req := req.Clone(context.Background())
			req.URL.Scheme = "http"
			req.Header.Del("Authorization")
			resp, httpErr := hr.httpDoer.Do(req)

			// clear err since the fallback succeeded
			if httpErr == nil {
				err = nil
			}
			discardHTTPRespBody(resp)
		}
		return err
	}

	// Deprecated code path.
	var port int
	if handler.HTTPGet.Port.Type == intstr.String && len(handler.HTTPGet.Port.StrVal) == 0 {
		port = 80
	} else {
		var err error
		port, err = resolvePort(handler.HTTPGet.Port, container)
		if err != nil {
			return err
		}
	}

	url := fmt.Sprintf("http://%s/%s", net.JoinHostPort(host, strconv.Itoa(port)), handler.HTTPGet.Path)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := hr.httpDoer.Do(req)

	discardHTTPRespBody(resp)
	return err
}

func discardHTTPRespBody(resp *http.Response) {
	if resp == nil {
		return
	}

	// Ensure the response body is fully read and closed
	// before we reconnect, so that we reuse the same TCP
	// connection.
	defer resp.Body.Close()

	if resp.ContentLength <= maxRespBodyLength {
		io.Copy(io.Discard, &io.LimitedReader{R: resp.Body, N: maxRespBodyLength})
	}
}

// NewAppArmorAdmitHandler returns a PodAdmitHandler which is used to evaluate
// if a pod can be admitted from the perspective of AppArmor.
func NewAppArmorAdmitHandler(validator apparmor.Validator) PodAdmitHandler {
	return &appArmorAdmitHandler{
		Validator: validator,
	}
}

type appArmorAdmitHandler struct {
	apparmor.Validator
}

func (a *appArmorAdmitHandler) Admit(attrs *PodAdmitAttributes) PodAdmitResult {
	// If the pod is already running or terminated, no need to recheck AppArmor.
	if attrs.Pod.Status.Phase != v1.PodPending {
		return PodAdmitResult{Admit: true}
	}

	err := a.Validate(attrs.Pod)
	if err == nil {
		return PodAdmitResult{Admit: true}
	}
	return PodAdmitResult{
		Admit:   false,
		Reason:  "AppArmor",
		Message: fmt.Sprintf("Cannot enforce AppArmor: %v", err),
	}
}

func isHTTPResponseError(err error) bool {
	if err == nil {
		return false
	}
	urlErr := &url.Error{}
	if !errors.As(err, &urlErr) {
		return false
	}
	return strings.Contains(urlErr.Err.Error(), "server gave HTTP response to HTTPS client")
}
