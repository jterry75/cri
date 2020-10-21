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
	"strconv"

	runhcsoptions "github.com/Microsoft/hcsshim/cmd/containerd-shim-runhcs-v1/options"
	"github.com/containerd/containerd"
	containerdio "github.com/containerd/containerd/cio"
	"github.com/containerd/containerd/containers"
	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/log"
	"github.com/containerd/containerd/oci"
	"github.com/containerd/containerd/snapshots"
	"github.com/davecgh/go-spew/spew"
	imagespec "github.com/opencontainers/image-spec/specs-go/v1"
	runtimespec "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	runtime "k8s.io/cri-api/pkg/apis/runtime/v1alpha2"

	"github.com/containerd/cri/pkg/annotations"
	criconfig "github.com/containerd/cri/pkg/config"
	customopts "github.com/containerd/cri/pkg/containerd/opts"
	ctrdutil "github.com/containerd/cri/pkg/containerd/util"
	"github.com/containerd/cri/pkg/netns"
	sandboxstore "github.com/containerd/cri/pkg/store/sandbox"
	"github.com/containerd/cri/pkg/util"
)

// RunPodSandbox creates and starts a pod-level sandbox. Runtimes should ensure
// the sandbox is in ready state.
func (c *criService) RunPodSandbox(ctx context.Context, r *runtime.RunPodSandboxRequest) (_ *runtime.RunPodSandboxResponse, retErr error) {
	config := r.GetConfig()
	log.G(ctx).Debugf("Sandbox config %+v", config)

	// Generate unique id and name for the sandbox and reserve the name.
	id := util.GenerateID()
	metadata := config.GetMetadata()
	if metadata == nil {
		return nil, errors.New("sandbox config must include metadata")
	}
	name := makeSandboxName(metadata)
	log.G(ctx).Debugf("Generated id %q for sandbox %q", id, name)
	// Reserve the sandbox name to avoid concurrent `RunPodSandbox` request starting the
	// same sandbox.
	if err := c.sandboxNameIndex.Reserve(name, id); err != nil {
		return nil, errors.Wrapf(err, "failed to reserve sandbox name %q", name)
	}
	defer func() {
		// Release the name if the function returns with an error.
		if retErr != nil {
			c.sandboxNameIndex.ReleaseByName(name)
		}
	}()

	// Create initial internal sandbox object.
	runtimeHandler := r.GetRuntimeHandler()
	sandbox := sandboxstore.NewSandbox(
		sandboxstore.Metadata{
			ID:             id,
			Name:           name,
			Config:         config,
			RuntimeHandler: runtimeHandler,
		},
		sandboxstore.Status{
			State: sandboxstore.StateUnknown,
		},
	)

	ociRuntime, err := c.getSandboxRuntime(config, runtimeHandler)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get sandbox runtime")
	}
	log.G(ctx).Debugf("Use OCI %+v for sandbox %q", ociRuntime, id)

	// Ensure sandbox container image snapshot.
	runtimeOpts, err := generateRuntimeOptions(ociRuntime, c.config)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate runtime options")
	}
	rhcso := runtimeOpts.(*runhcsoptions.Options)

	var imageName string
	if rhcso.SandboxImage != "" {
		imageName = rhcso.SandboxImage
	} else {
		imageName = c.config.SandboxImage
	}
	var sandboxPlatform string
	if rhcso.SandboxPlatform != "" {
		sandboxPlatform = rhcso.SandboxPlatform
	} else {
		sandboxPlatform = "windows/amd64"
	}
	// TODO JTERRY75: This is only required while we dont have a platform for
	// the image in the cri spec.
	if config.Labels == nil {
		config.Labels = make(map[string]string)
	}
	config.Labels["sandbox-platform"] = sandboxPlatform

	image, err := c.ensureImageExists(ctx, imageName, config)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get sandbox image %q", imageName)
	}
	containerdImage, err := c.toContainerdImage(ctx, *image)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get image from containerd %q", image.ID)
	}

	// If it is not in host network namespace then create a namespace and set the sandbox
	// handle. NetNSPath in sandbox metadata and NetNS is non empty only for non host network
	// namespaces. If the pod is in host network namespace then both are empty and should not
	// be used.
	sandbox.NetNS, err = netns.NewNetNS()
	if err != nil {
		return nil, errors.Wrapf(err, "failed to create network namespace for sandbox %q", id)
	}
	sandbox.NetNSPath = sandbox.NetNS.GetPath()
	defer func() {
		if retErr != nil {
			if err := sandbox.NetNS.Remove(); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to remove network namespace %s for sandbox %q", sandbox.NetNSPath, id)
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
	sandbox.IP, sandbox.CNIResult, err = c.setupPod(ctx, id, sandbox.NetNSPath, config)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to setup network for sandbox %q", id)
	}
	defer func() {
		if retErr != nil {
			// Teardown network if an error is returned.
			if err := c.teardownPod(id, sandbox.NetNSPath, config); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to destroy network for sandbox %q", id)
			}
		}
	}()

	// Create sandbox container.
	spec, err := c.generateSandboxContainerSpec(id, config, sandboxPlatform, &image.ImageSpec.Config, sandbox.NetNSPath, rhcso.SandboxIsolation)
	if err != nil {
		return nil, errors.Wrap(err, "failed to generate sandbox container spec")
	}
	log.G(ctx).Debugf("Sandbox container %q spec: %#+v", id, spew.NewFormatter(spec))

	sandboxLabels := buildLabels(config.Labels, containerKindSandbox)

	// If the config field is specified, set the snapshotter label to reuse the pods
	// scratch space. This only affects LCOW at the moment.
	if rhcso.ShareScratch {
		config.Annotations["containerd.io/snapshot/io.microsoft.container.storage.reuse-scratch"] = "true"
		config.Annotations["containerd.io/snapshot/io.microsoft.container.storage.reuse-scratch.container-type"] = "sandbox"
		config.Annotations["containerd.io/snapshot/io.microsoft.sandbox.id"] = id
	}

	if rhcso.DefaultContainerScratchSizeInGb != 0 {
		size := strconv.FormatInt(int64(rhcso.DefaultContainerScratchSizeInGb), 10)
		config.Annotations["containerd.io/snapshot/io.microsoft.container.storage.rootfs.size-gb"] = size
	}

	if rhcso.DefaultVmScratchSizeInGb != 0 {
		size := strconv.FormatInt(int64(rhcso.DefaultVmScratchSizeInGb), 10)
		config.Annotations["containerd.io/snapshot/io.microsoft.vm.storage.rootfs.size-gb"] = size
	}

	snapshotterOpt := snapshots.WithLabels(config.Annotations)

	opts := []containerd.NewContainerOpts{
		containerd.WithImage(containerdImage),
		containerd.WithSnapshotter(c.getDefaultSnapshotterForPlatform(sandboxPlatform)),
		customopts.WithNewSnapshot(id, containerdImage, snapshotterOpt),
		containerd.WithContainerLabels(sandboxLabels),
		containerd.WithContainerExtension(sandboxMetadataExtension, &sandbox.Metadata),
		containerd.WithSpec(spec),
		containerd.WithRuntime(ociRuntime.Type, runtimeOpts)}

	container, err := c.client.NewContainer(ctx, id, opts...)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create containerd container")
	}
	defer func() {
		if retErr != nil {
			deferCtx, deferCancel := ctrdutil.DeferContext()
			defer deferCancel()
			if err := container.Delete(deferCtx, containerd.WithSnapshotCleanup); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to delete containerd container %q", id)
			}
		}
	}()

	// Create sandbox container root directories.
	sandboxRootDir := c.getSandboxRootDir(id)
	if err := c.os.MkdirAll(sandboxRootDir, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to create sandbox root directory %q",
			sandboxRootDir)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the sandbox root directory.
			if err := c.os.RemoveAll(sandboxRootDir); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to remove sandbox root directory %q",
					sandboxRootDir)
			}
		}
	}()
	volatileSandboxRootDir := c.getVolatileSandboxRootDir(id)
	if err := c.os.MkdirAll(volatileSandboxRootDir, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to create volatile sandbox root directory %q",
			volatileSandboxRootDir)
	}
	defer func() {
		if retErr != nil {
			// Cleanup the volatile sandbox root directory.
			if err := c.os.RemoveAll(volatileSandboxRootDir); err != nil {
				log.G(ctx).WithError(err).Errorf("Failed to remove volatile sandbox root directory %q",
					volatileSandboxRootDir)
			}
		}
	}()

	// Update sandbox created timestamp.
	info, err := container.Info(ctx)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get sandbox container info")
	}

	// Create sandbox task in containerd.
	log.G(ctx).Tracef("Create sandbox container (id=%q, name=%q).",
		id, name)

	// We don't need stdio for sandbox container.
	task, err := container.NewTask(ctx, containerdio.NullIO)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create containerd task")
	}
	defer func() {
		if retErr != nil {
			deferCtx, deferCancel := ctrdutil.DeferContext()
			defer deferCancel()
			// Cleanup the sandbox container if an error is returned.
			if _, err := task.Delete(deferCtx, containerd.WithProcessKill); err != nil && !errdefs.IsNotFound(err) {
				log.G(ctx).WithError(err).Errorf("Failed to delete sandbox container %q", id)
			}
		}
	}()

	// wait is a long running background request, no timeout needed.
	exitCh, err := task.Wait(ctrdutil.NamespacedContext())
	if err != nil {
		return nil, errors.Wrap(err, "failed to wait for sandbox container task")
	}

	if err := task.Start(ctx); err != nil {
		return nil, errors.Wrapf(err, "failed to start sandbox container task %q", id)
	}

	if err := sandbox.Status.Update(func(status sandboxstore.Status) (sandboxstore.Status, error) {
		// Set the pod sandbox as ready after successfully start sandbox container.
		status.Pid = task.Pid()
		status.State = sandboxstore.StateReady
		status.CreatedAt = info.CreatedAt
		return status, nil
	}); err != nil {
		return nil, errors.Wrap(err, "failed to update sandbox status")
	}

	// Add sandbox into sandbox store in INIT state.
	sandbox.Container = container

	if err := c.sandboxStore.Add(sandbox); err != nil {
		return nil, errors.Wrapf(err, "failed to add sandbox %+v into store", sandbox)
	}

	// start the monitor after adding sandbox into the store, this ensures
	// that sandbox is in the store, when event monitor receives the TaskExit event.
	//
	// TaskOOM from containerd may come before sandbox is added to store,
	// but we don't care about sandbox TaskOOM right now, so it is fine.
	c.eventMonitor.startExitMonitor(context.Background(), id, task.Pid(), exitCh)

	return &runtime.RunPodSandboxResponse{PodSandboxId: id}, nil
}

func (c *criService) generateSandboxContainerSpec(id string, config *runtime.PodSandboxConfig, sandboxPlatform string,
	imageConfig *imagespec.ImageConfig, nsPath string, isolation runhcsoptions.Options_SandboxIsolation) (*runtimespec.Spec, error) {
	ctx := ctrdutil.NamespacedContext()
	spec, err := oci.GenerateSpecWithPlatform(ctx, nil, sandboxPlatform, &containers.Container{ID: id})
	if err != nil {
		return nil, err
	}
	g := newSpecGenerator(spec)

	// Apply default config from image config.
	if err := addImageEnvs(&g, imageConfig.Env); err != nil {
		return nil, err
	}

	if imageConfig.WorkingDir != "" {
		g.SetProcessCwd(imageConfig.WorkingDir)
	}

	if len(imageConfig.Entrypoint) == 0 && len(imageConfig.Cmd) == 0 {
		// Pause image must have entrypoint or cmd.
		return nil, errors.Errorf("invalid empty entrypoint and cmd in image config %+v", imageConfig)
	}
	// Set process commands.
	g.SetProcessArgs(append(imageConfig.Entrypoint, imageConfig.Cmd...))

	// Clear the root location since runhcs sets it on the mount path in the
	// guest.
	g.Config.Root = nil

	if isolation == runhcsoptions.Options_HYPERVISOR {
		// TODO: JTERRY75 - This is a hack. Setting to the empty string will
		// initialize the Windows.HyperV section which is really all we want.
		g.SetWindowsHypervUntilityVMPath("")
	}
	g.SetWindowsNetworkNamespace(nsPath)

	// Set hostname.
	g.SetHostname(config.GetHostname())

	if sandboxPlatform == "linux/amd64" {
		// Set cgroups parent.
		if c.config.DisableCgroup {
			g.SetLinuxCgroupsPath("")
		} else {
			if config.GetLinux().GetCgroupParent() != "" {
				return nil, errors.New("lcow does not support custom cgroup parents")
			}
		}

		g.SetProcessUsername(imageConfig.User)

		securityContext := config.GetLinux().GetSecurityContext()
		userstr, err := generateUserString(
			"",
			securityContext.GetRunAsUser(),
			securityContext.GetRunAsGroup())
		if err != nil {
			return nil, errors.Wrap(err, "failed to generate user string")
		}
		if userstr == "" {
			// Lastly, since no user override was passed via CRI try to set via
			// OCI Image
			userstr = imageConfig.User
		}
		if userstr != "" {
			g.AddAnnotation("io.microsoft.lcow.userstr", userstr)
		}

		for _, group := range securityContext.GetSupplementalGroups() {
			g.AddProcessAdditionalGid(uint32(group))
		}
	}

	// Forward any annotations from the orchestrator
	for k, v := range config.Annotations {
		g.AddAnnotation(k, v)
	}

	// Apply forcibly the sandbox annotations for this POD
	g.AddAnnotation(annotations.ContainerType, annotations.ContainerTypeSandbox)
	g.AddAnnotation(annotations.SandboxID, id)

	return g.Config, nil
}

// getSandboxRuntime returns the runtime configuration for sandbox.
// If the sandbox contains untrusted workload, runtime for untrusted workload will be returned,
// or else default runtime will be returned.
func (c *criService) getSandboxRuntime(config *runtime.PodSandboxConfig, runtimeHandler string) (criconfig.Runtime, error) {
	if runtimeHandler == "" {
		return c.config.ContainerdConfig.DefaultRuntime, nil
	}
	handler, ok := c.config.ContainerdConfig.Runtimes[runtimeHandler]
	if !ok {
		return criconfig.Runtime{}, errors.Errorf("no runtime for %q is configured", runtimeHandler)
	}
	return handler, nil
}

// unmountSandboxFiles is a noop on Windows as there is nothing that was mounted.
func (c *criService) unmountSandboxFiles(id string, config *runtime.PodSandboxConfig) error {
	return nil
}
