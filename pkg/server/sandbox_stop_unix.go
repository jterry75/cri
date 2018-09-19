// +build !windows

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
	"os"

	sandboxstore "github.com/containerd/cri/pkg/store/sandbox"
	cni "github.com/containerd/go-cni"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
)

func (c *criService) doStopPodSandbox(id string, sandbox sandboxstore.Sandbox) error {
	// Teardown network for sandbox.
	if sandbox.NetNSPath != "" && sandbox.NetNS != nil {
		if _, err := os.Stat(sandbox.NetNSPath); err != nil {
			if !os.IsNotExist(err) {
				return errors.Wrapf(err, "failed to stat network namespace path %s", sandbox.NetNSPath)
			}
		} else {
			if teardownErr := c.teardownPod(id, sandbox.NetNSPath, sandbox.Config); teardownErr != nil {
				return errors.Wrapf(teardownErr, "failed to destroy network for sandbox %q", id)
			}
		}
		/*TODO:It is still possible that containerd crashes after we teardown the network, but before we remove the network namespace.
		In that case, we'll not be able to remove the sandbox anymore. The chance is slim, but we should be aware of that.
		In the future, once TearDownPod is idempotent, this will be fixed.*/

		//Close the sandbox network namespace if it was created
		if err := sandbox.NetNS.Remove(); err != nil {
			return errors.Wrapf(err, "failed to remove network namespace for sandbox %q", id)
		}
	}

	logrus.Infof("TearDown network for sandbox %q successfully", id)

	if err := c.unmountSandboxFiles(id, sandbox.Config); err != nil {
		return errors.Wrap(err, "failed to unmount sandbox files")
	}

	return nil
}

// teardownPod removes the network from the pod
func (c *criService) teardownPod(id string, path string, config *runtime.PodSandboxConfig) error {
	if c.netPlugin == nil {
		return errors.New("cni config not intialized")
	}

	labels := getPodCNILabels(id, config)
	return c.netPlugin.Remove(id,
		path,
		cni.WithLabels(labels),
		cni.WithCapabilityPortMap(toCNIPortMappings(config.GetPortMappings())))
}
