/*
Copyright 2017 The Kubernetes Authors.

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

package server

import (
	"fmt"
	"strings"

	cni "github.com/containerd/go-cni"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"

	"github.com/containerd/cri/pkg/annotations"
	sandboxstore "github.com/containerd/cri/pkg/store/sandbox"
)

// parseDNSOptions parse DNS options into resolv.conf format content,
// if none option is specified, will return empty with no error.
func parseDNSOptions(servers, searches, options []string) (string, error) {
	resolvContent := ""

	if len(searches) > maxDNSSearches {
		return "", errors.New("DNSOption.Searches has more than 6 domains")
	}

	if len(searches) > 0 {
		resolvContent += fmt.Sprintf("search %s\n", strings.Join(searches, " "))
	}

	if len(servers) > 0 {
		resolvContent += fmt.Sprintf("nameserver %s\n", strings.Join(servers, "\nnameserver "))
	}

	if len(options) > 0 {
		resolvContent += fmt.Sprintf("options %s\n", strings.Join(options, " "))
	}

	return resolvContent, nil
}

func (c *criService) setupPodNetwork(sandbox *sandboxstore.Sandbox) (retErr error) {
	id := sandbox.Metadata.ID
	netns, err := sandboxstore.NewNetNS()
	if err != nil {
		return errors.Wrapf(err, "failed to create network namespace for sandbox %q", id)
	}
	sandbox.NetNS = netns
	config := sandbox.Metadata.Config
	sandbox.NetNSPath = sandbox.NetNS.GetPath()
	defer func() {
		if retErr != nil {
			if err := sandbox.NetNS.Remove(); err != nil {
				logrus.WithError(err).Errorf("Failed to remove network namespace %s for sandbox %q", sandbox.NetNSPath, id)
			}
			sandbox.NetNSPath = ""
		}
	}()
	// Setup network for sandbox.
	// Certain VM based solutions like clear containers (Issue containerd/cri-containerd#524)
	// rely on the assumption that CRI shim will not be querying the network namespace to check the
	// network states such as IP.
	// In future runtime implementation should avoid relying on CRI shim implementation details.
	// In this case however caching the IP will add a subtle performance enhancement by avoiding
	// calls to network namespace of the pod to query the IP of the veth interface on every
	// SandboxStatus request.
	sandbox.IP, err = c.setupPod(id, sandbox.NetNSPath, config)
	if err != nil {
		return errors.Wrapf(err, "failed to setup network for sandbox %q", id)
	}
	defer func() {
		if retErr != nil {
			// Teardown network if an error is returned.
			if err := c.teardownPod(id, sandbox.NetNSPath, config); err != nil {
				logrus.WithError(err).Errorf("Failed to destroy network for sandbox %q", id)
			}
		}
	}()

	return nil
}

// setupPod setups up the network for a pod
func (c *criService) setupPod(id string, path string, config *runtime.PodSandboxConfig) (string, error) {
	if c.netPlugin == nil {
		return "", errors.New("cni config not intialized")
	}

	labels := getPodCNILabels(id, config)
	result, err := c.netPlugin.Setup(id,
		path,
		cni.WithLabels(labels),
		cni.WithCapabilityPortMap(toCNIPortMappings(config.GetPortMappings())))
	if err != nil {
		return "", err
	}
	// Check if the default interface has IP config
	if configs, ok := result.Interfaces[defaultIfName]; ok && len(configs.IPConfigs) > 0 {
		return selectPodIP(configs.IPConfigs), nil
	}
	// If it comes here then the result was invalid so destroy the pod network and return error
	if err := c.teardownPod(id, path, config); err != nil {
		logrus.WithError(err).Errorf("Failed to destroy network for sandbox %q", id)
	}
	return "", errors.Errorf("failed to find network info for sandbox %q", id)
}

// toCNIPortMappings converts CRI port mappings to CNI.
func toCNIPortMappings(criPortMappings []*runtime.PortMapping) []cni.PortMapping {
	var portMappings []cni.PortMapping
	for _, mapping := range criPortMappings {
		if mapping.HostPort <= 0 {
			continue
		}
		portMappings = append(portMappings, cni.PortMapping{
			HostPort:      mapping.HostPort,
			ContainerPort: mapping.ContainerPort,
			Protocol:      strings.ToLower(mapping.Protocol.String()),
			HostIP:        mapping.HostIp,
		})
	}
	return portMappings
}

// selectPodIP select an ip from the ip list. It prefers ipv4 more than ipv6.
func selectPodIP(ipConfigs []*cni.IPConfig) string {
	for _, c := range ipConfigs {
		if c.IP.To4() != nil {
			return c.IP.String()
		}
	}
	return ipConfigs[0].IP.String()
}

// untrustedWorkload returns true if the sandbox contains untrusted workload.
func untrustedWorkload(config *runtime.PodSandboxConfig) bool {
	return config.GetAnnotations()[annotations.UntrustedWorkload] == "true"
}

// hostAccessingSandbox returns true if the sandbox configuration
// requires additional host access for the sandbox.
func hostAccessingSandbox(config *runtime.PodSandboxConfig) bool {
	securityContext := config.GetLinux().GetSecurityContext()

	namespaceOptions := securityContext.GetNamespaceOptions()
	if namespaceOptions.GetNetwork() == runtime.NamespaceMode_NODE ||
		namespaceOptions.GetPid() == runtime.NamespaceMode_NODE ||
		namespaceOptions.GetIpc() == runtime.NamespaceMode_NODE {
		return true
	}

	return false
}
