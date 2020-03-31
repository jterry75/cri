// +build windows

/*
Copyright The containerd Authors.

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

package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	"github.com/containerd/containerd"
	"github.com/pkg/errors"
	"k8s.io/kubernetes/pkg/kubelet/server/streaming"
)

// DefaultConfig returns default configurations of cri plugin.
func DefaultConfig() PluginConfig {
	rs5Opts := options.Options{
		SandboxImage:     "mcr.microsoft.com/windows/nanoserver:1809",
		SandboxPlatform:  "windows/amd64",
		SandboxIsolation: options.Options_HYPERVISOR,
	}
	v19H1Opts := options.Options{
		SandboxImage:     "mcr.microsoft.com/windows/nanoserver:1903",
		SandboxPlatform:  "windows/amd64",
		SandboxIsolation: options.Options_HYPERVISOR,
	}
	return PluginConfig{
		CniConfig: CniConfig{
			NetworkPluginBinDir:       filepath.Join(os.Getenv("ProgramFiles"), "containerd", "cni", "bin"),
			NetworkPluginConfDir:      filepath.Join(os.Getenv("ProgramFiles"), "containerd", "cni", "conf"),
			NetworkPluginMaxConfNum:   1,
			NetworkPluginConfTemplate: "",
		},
		ContainerdConfig: ContainerdConfig{
			Snapshotter:        containerd.DefaultSnapshotter,
			DefaultRuntimeName: "runhcs-wcow-process",
			NoPivot:            false,
			Runtimes: map[string]Runtime{
				"runhcs-wcow-process": {
					Type:                 "io.containerd.runhcs.v1",
					ContainerAnnotations: []string{"io.microsoft.container.*"},
				},
				"runhcs-wcow-hypervisor-1809": {
					Type:                 "io.containerd.runhcs.v1",
					PodAnnotations:       []string{"io.microsoft.virtualmachine.*"},
					ContainerAnnotations: []string{"io.microsoft.container.*"},
					Options:              rs5Opts,
				},
				"runhcs-wcow-hypervisor-17763": {
					Type:                 "io.containerd.runhcs.v1",
					PodAnnotations:       []string{"io.microsoft.virtualmachine.*"},
					ContainerAnnotations: []string{"io.microsoft.container.*"},
					Options:              rs5Opts,
				},
				"runhcs-wcow-hypervisor-1903": {
					Type:                 "io.containerd.runhcs.v1",
					PodAnnotations:       []string{"io.microsoft.virtualmachine.*"},
					ContainerAnnotations: []string{"io.microsoft.container.*"},
					Options:              v19H1Opts,
				},
				"runhcs-wcow-hypervisor-18362": {
					Type:                 "io.containerd.runhcs.v1",
					PodAnnotations:       []string{"io.microsoft.virtualmachine.*"},
					ContainerAnnotations: []string{"io.microsoft.container.*"},
					Options:              v19H1Opts,
				},
			},
		},
		DisableTCPService:   true,
		StreamServerAddress: "127.0.0.1",
		StreamServerPort:    "0",
		StreamIdleTimeout:   streaming.DefaultConfig.StreamIdleTimeout.String(), // 4 hour
		EnableTLSStreaming:  false,
		X509KeyPairStreaming: X509KeyPairStreaming{
			TLSKeyFile:  "",
			TLSCertFile: "",
		},
		SandboxImage:            "mcr.microsoft.com/k8s/core/pause:1.2.0",
		StatsCollectPeriod:      10,
		MaxContainerLogLineSize: 16 * 1024,
		Registry: Registry{
			Mirrors: map[string]Mirror{
				"docker.io": {
					Endpoints: []string{"https://registry-1.docker.io"},
				},
			},
		},
		MaxConcurrentDownloads: 3,
		// TODO(windows): Add platform specific config, so that most common defaults can be shared.
	}
}

// fixupPlatformConfig allows the platform to fixup a Runtime.Options config
// that was parsed by toml. Because the Runtime.Options is of type `interface{}`
// toml will unmarshal this as `map[string]interface{}` when loading from a
// config file. However, when not loaded from a config file the default config
// will be of type *options.Options. This fixup allows a runtime to properly
// change type type back such that all runtime.Options access later can assume
// the type.
func fixupPlatformConfig(c *PluginConfig) error {
	for n, r := range c.ContainerdConfig.Runtimes {
		if r.Options != nil {
			if m, ok := r.Options.(map[string]interface{}); ok {
				out, err := json.Marshal(m)
				if err != nil {
					return errors.Wrapf(err, "failed to marshal runtime %s options to json", n)
				}
				opts := options.Options{}
				err = json.Unmarshal(out, &opts)
				if err != nil {
					return errors.Wrapf(err, "failed to unmarshal runtime %s options from json %s", n, out)
				}
				r.Options = opts
				c.ContainerdConfig.Runtimes[n] = r
			}
		}
	}
	return nil
}
