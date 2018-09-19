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
	"github.com/containerd/containerd/containers"
	criconfig "github.com/containerd/cri/pkg/config"
	"github.com/pkg/errors"
	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
)

// initSelinuxOpts is not supported on Windows.
func initSelinuxOpts(selinuxOpt *runtime.SELinuxOption) (string, string, error) {
	return "", "", nil
}

// getRuntimeConfigFromContainerInfo gets runtime configuration from containerd
// container info.
func getRuntimeConfigFromContainerInfo(c containers.Container) (criconfig.Runtime, error) {
	r := criconfig.Runtime{
		Type: c.Runtime.Name,
	}
	if c.Runtime.Options != nil {
		// CRI plugin makes sure that runtime option is not used on Windows curently.
		return criconfig.Runtime{}, errors.New("runtime options is not nil")
	}
	return r, nil
}
