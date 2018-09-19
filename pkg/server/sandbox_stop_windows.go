// +build windows

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
	sandboxstore "github.com/containerd/cri/pkg/store/sandbox"
	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
)

func (c *criService) doStopPodSandbox(id string, sandbox sandboxstore.Sandbox) error {
	return nil
}

func (c *criService) teardownPod(id string, path string, config *runtime.PodSandboxConfig) error {
	return nil
}
