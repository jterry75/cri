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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/containerd/containerd"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/oci"
	runhcsoptions "github.com/containerd/containerd/runtime/v2/runhcs/options"
	"github.com/containerd/typeurl"
	"github.com/davecgh/go-spew/spew"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/opencontainers/runtime-tools/generate"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/net/context"
	runtime "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"

	"github.com/containerd/cri/pkg/annotations"
	customopts "github.com/containerd/cri/pkg/containerd/opts"
	ctrdutil "github.com/containerd/cri/pkg/containerd/util"
	cio "github.com/containerd/cri/pkg/server/io"
	containerstore "github.com/containerd/cri/pkg/store/container"
	"github.com/containerd/cri/pkg/util"
)

func init() {
	typeurl.Register(&containerstore.Metadata{},
		"github.com/containerd/cri/pkg/store/container", "Metadata")
}

// CreateContainer creates a new container in the given PodSandbox.
func (c *criService) CreateContainer(ctx context.Context, r *runtime.CreateContainerRequest) (_ *runtime.CreateContainerResponse, retErr error) {
	config := r.GetConfig()
	sandboxConfig := r.GetSandboxConfig()
	sandbox, err := c.sandboxStore.Get(r.GetPodSandboxId())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to find sandbox id %q", r.GetPodSandboxId())
	}
	sandboxID := sandbox.ID
	s, err := sandbox.Container.Task(ctx, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get sandbox container task")
	}
	sandboxPid := s.Pid()

	// Generate unique id and name for the container and reserve the name.
	// Reserve the container name to avoid concurrent `CreateContainer` request creating
	// the same container.
	id := util.GenerateID()
	metadata := config.GetMetadata()
	if metadata == nil {
		return nil, errors.New("container config must include metadata")
	}
	name := makeContainerName(metadata, sandboxConfig.GetMetadata())
	logrus.Debugf("Generated id %q for container %q", id, name)
	if err = c.containerNameIndex.Reserve(name, id); err != nil {
		return nil, errors.Wrapf(err, "failed to reserve container name %q", name)
	}
	defer func() {
		// Release the name if the function returns with an error.
		if retErr != nil {
			c.containerNameIndex.ReleaseByName(name)
		}
	}()

	// Create initial internal container metadata.
	meta := containerstore.Metadata{
		ID:        id,
		Name:      name,
		SandboxID: sandboxID,
		Config:    config,
	}

	// Prepare container image snapshot. For container, the image should have
	// been pulled before creating the container, so do not ensure the image.
	image, err := c.localResolve(config.GetImage().GetImage())
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve image %q", config.GetImage().GetImage())
	}

	// Run container using the same runtime with sandbox.
	sandboxInfo, err := sandbox.Container.Info(ctx)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get sandbox %q info", sandboxID)
	}

	// Create container root directory.
	containerRootDir := c.getContainerRootDir(id)
	if err = c.os.MkdirAll(containerRootDir, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to create container root directory %q",
			containerRootDir)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the container root directory.
			if err = c.os.RemoveAll(containerRootDir); err != nil {
				logrus.WithError(err).Errorf("Failed to remove container root directory %q",
					containerRootDir)
			}
		}
	}()
	volatileContainerRootDir := c.getVolatileContainerRootDir(id)
	if err = c.os.MkdirAll(volatileContainerRootDir, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to create volatile container root directory %q",
			volatileContainerRootDir)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the volatile container root directory.
			if err = c.os.RemoveAll(volatileContainerRootDir); err != nil {
				logrus.WithError(err).Errorf("Failed to remove volatile container root directory %q",
					volatileContainerRootDir)
			}
		}
	}()

	var sandboxPlatform string
	if sandbox.RuntimeHandler != "" {
		// Get the RuntimeHandler config overrides
		ociRuntime := c.config.Runtimes[sandbox.RuntimeHandler]
		runtimeOpts, err := generateRuntimeOptions(ociRuntime, c.config)
		if err != nil {
			return nil, errors.Wrap(err, "failed to generate runtime options")
		}
		rhcso := runtimeOpts.(*runhcsoptions.Options)
		sandboxPlatform = rhcso.SandboxPlatform
	}
	if sandboxPlatform == "" {
		sandboxPlatform = "windows/amd64"
	}

	// Create container volumes mounts.
	volumeMounts := c.generateVolumeMounts(containerRootDir, config.GetMounts(), &image.ImageSpec.Config)

	spec, err := c.generateContainerSpec(id, sandboxID, sandboxPid, sandbox.NetNSPath, config, sandboxConfig, sandboxPlatform, &image.ImageSpec.Config, volumeMounts)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to generate container %q spec", id)
	}

	logrus.Debugf("Container %q spec: %#+v", id, spew.NewFormatter(spec))

	// Set snapshotter before any other options.
	opts := []containerd.NewContainerOpts{
		containerd.WithSnapshotter(c.getDefaultSnapshotterForPlatform(sandboxPlatform)),
		customopts.WithNewSnapshot(id, image.Image),
	}

	if len(volumeMounts) > 0 {
		mountMap := make(map[string]string)
		for _, v := range volumeMounts {
			mountMap[v.HostPath] = v.ContainerPath
		}
		opts = append(opts, customopts.WithVolumes(mountMap))
	}

	meta.ImageRef = image.ID
	meta.StopSignal = image.ImageSpec.Config.StopSignal

	// Get container log path.
	if config.GetLogPath() != "" {
		meta.LogPath = filepath.Join(sandbox.Config.GetLogDirectory(), config.GetLogPath())
	}

	containerIO, err := cio.NewContainerIO(id,
		cio.WithNewFIFOs(volatileContainerRootDir, config.GetTty(), config.GetStdin()))
	if err != nil {
		return nil, errors.Wrap(err, "failed to create container io")
	}
	defer func() {
		if retErr != nil {
			if err := containerIO.Close(); err != nil {
				logrus.WithError(err).Errorf("Failed to close container io %q", id)
			}
		}
	}()

	containerLabels := buildLabels(config.Labels, containerKindContainer)
	runtimeOptions, err := getRuntimeOptions(sandboxInfo)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get runtime options")
	}
	opts = append(opts,
		containerd.WithSpec(spec),
		containerd.WithContainerLabels(containerLabels),
		containerd.WithContainerExtension(containerMetadataExtension, &meta),
		containerd.WithRuntime(sandboxInfo.Runtime.Name, runtimeOptions))

	var cntr containerd.Container
	if cntr, err = c.client.NewContainer(ctx, id, opts...); err != nil {
		return nil, errors.Wrap(err, "failed to create containerd container")
	}
	defer func() {
		if retErr != nil {
			deferCtx, deferCancel := ctrdutil.DeferContext()
			defer deferCancel()
			if err := cntr.Delete(deferCtx, containerd.WithSnapshotCleanup); err != nil {
				logrus.WithError(err).Errorf("Failed to delete containerd container %q", id)
			}
		}
	}()

	status := containerstore.Status{CreatedAt: time.Now().UnixNano()}
	container, err := containerstore.NewContainer(meta,
		containerstore.WithStatus(status, containerRootDir),
		containerstore.WithContainer(cntr),
		containerstore.WithContainerIO(containerIO),
	)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create internal container object for %q", id)
	}
	defer func() {
		if retErr != nil {
			// Cleanup container checkpoint on error.
			if err := container.Delete(); err != nil {
				logrus.WithError(err).Errorf("Failed to cleanup container checkpoint for %q", id)
			}
		}
	}()

	// Add container into container store.
	if err := c.containerStore.Add(container); err != nil {
		return nil, errors.Wrapf(err, "failed to add container %q into store", id)
	}

	return &runtime.CreateContainerResponse{ContainerId: id}, nil
}

func (c *criService) generateContainerSpec(id string, sandboxID string, sandboxPid uint32, netnsPath string, config *runtime.ContainerConfig,
	sandboxConfig *runtime.PodSandboxConfig, sandboxPlatform string, imageConfig *imagespec.ImageConfig, extraMounts []*runtime.Mount) (*runtimespec.Spec, error) {
	// Creates a spec Generator with the default spec.
	ctx := ctrdutil.NamespacedContext()
	spec, err := oci.GenerateSpecWithPlatform(ctx, nil, sandboxPlatform, &containers.Container{ID: id})
	if err != nil {
		return nil, err
	}
	g := newSpecGenerator(spec)

	if err := setOCIProcessArgs(&g, config, imageConfig); err != nil {
		return nil, err
	}

	if config.GetWorkingDir() != "" {
		g.SetProcessCwd(config.GetWorkingDir())
	} else if imageConfig.WorkingDir != "" {
		g.SetProcessCwd(imageConfig.WorkingDir)
	}

	g.SetProcessTerminal(config.GetTty())

	// Apply envs from image config first, so that envs from container config
	// can override them.
	if err := addImageEnvs(&g, imageConfig.Env); err != nil {
		return nil, err
	}
	for _, e := range config.GetEnvs() {
		g.AddProcessEnv(e.GetKey(), e.GetValue())
	}

	// Merge extra mounts and CRI mounts.
	mounts := mergeMounts(config.GetMounts(), extraMounts)
	if err := c.addOCIBindMounts(&g, mounts); err != nil {
		return nil, errors.Wrapf(err, "failed to set OCI bind mounts %+v", mounts)
	}

	// Clear the root location since runhcs sets it on the mount path in the
	// guest.
	g.Config.Root = nil

	// Set the Network Namespace
	g.SetWindowsNetworkNamespace(netnsPath)

	g.AddAnnotation(annotations.ContainerType, annotations.ContainerTypeContainer)
	g.AddAnnotation(annotations.SandboxID, sandboxID)

	return g.Config, nil
}

// addOCIBindMounts adds bind mounts.
func (c *criService) addOCIBindMounts(g *generate.Generator, mounts []*runtime.Mount) error {
	// Sort mounts in number of parts. This ensures that high level mounts don't
	// shadow other mounts.
	sort.Sort(orderedMounts(mounts))

	// Copy all mounts from default mounts, except for
	// - mounts overriden by supplied mount;
	// - all mounts under /dev if a supplied /dev is present.
	mountSet := make(map[string]struct{})
	for _, m := range mounts {
		mountSet[filepath.Clean(m.ContainerPath)] = struct{}{}
	}
	defaultMounts := g.Mounts()
	g.ClearMounts()
	for _, m := range defaultMounts {
		dst := filepath.Clean(m.Destination)
		if _, ok := mountSet[dst]; ok {
			// filter out mount overridden by a supplied mount
			continue
		}
		if _, mountDev := mountSet["/dev"]; mountDev && strings.HasPrefix(dst, "/dev/") {
			// filter out everything under /dev if /dev is a supplied mount
			continue
		}
		g.AddMount(m)
	}

	for _, mount := range mounts {
		dst := mount.GetContainerPath()
		src := mount.GetHostPath()
		// Create the host path if it doesn't exist.
		if _, err := c.os.Stat(src); err != nil {
			if !os.IsNotExist(err) {
				return errors.Wrapf(err, "failed to stat %q", src)
			}
			if err := c.os.MkdirAll(src, 0755); err != nil {
				return errors.Wrapf(err, "failed to mkdir %q", src)
			}
		}
		src, err := c.os.ResolveSymbolicLink(src)
		if err != nil {
			return errors.Wrapf(err, "failed to resolve symlink %q", src)
		}

		options := []string{}
		if mount.GetReadonly() {
			options = append(options, "ro")
		} else {
			options = append(options, "rw")
		}

		g.AddMount(runtimespec.Mount{
			Source:      src,
			Destination: dst,
			Options:     options,
		})
	}

	return nil
}