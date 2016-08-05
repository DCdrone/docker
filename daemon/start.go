package daemon

import (
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"syscall"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/container"
	"github.com/docker/docker/errors"
	"github.com/docker/docker/libcontainerd"
	"github.com/docker/docker/runconfig"
	containertypes "github.com/docker/engine-api/types/container"
)

// ContainerStart starts a container.
//该方法主要做一些检查工作，具体的创建请见containerStart()。
func (daemon *Daemon) ContainerStart(name string, hostConfig *containertypes.HostConfig) error {

	//根据传入的容器名称查找容器是否存在。
	container, err := daemon.GetContainer(name)
	if err != nil {
		return err
	}

	//检查容器是否被paused了
	if container.IsPaused() {
		return fmt.Errorf("Cannot start a paused container, try unpause instead.")
	}

	//检查容器是否正在运行。
	if container.IsRunning() {
		err := fmt.Errorf("Container already started")
		return errors.NewErrorWithStatusCode(err, http.StatusNotModified)
	}

	// Windows does not have the backwards compatibility issue here.
	//以后将不可以在容器启动的时候设置hostConfig。包括网络模式等。
	//在进行一些参数验证(端口映射的设置、验证exec driver、验证内核是否支持cpu share，IO weight等)后
	if runtime.GOOS != "windows" {
		// This is kept for backward compatibility - hostconfig should be passed when
		// creating a container, not during start.
		if hostConfig != nil {
			logrus.Warn("DEPRECATED: Setting host configuration options when the container starts is deprecated and will be removed in Docker 1.12")
			oldNetworkMode := container.HostConfig.NetworkMode
			if err := daemon.setSecurityOptions(container, hostConfig); err != nil {
				return err
			}
			if err := daemon.setHostConfig(container, hostConfig); err != nil {
				return err
			}
			newNetworkMode := container.HostConfig.NetworkMode
			if string(oldNetworkMode) != string(newNetworkMode) {
				// if user has change the network mode on starting, clean up the
				// old networks. It is a deprecated feature and will be removed in Docker 1.12
				container.NetworkSettings.Networks = nil
				if err := container.ToDisk(); err != nil {
					return err
				}
			}
			container.InitDNSHostConfig()
		}
	} else {
		if hostConfig != nil {
			return fmt.Errorf("Supplying a hostconfig on start is not supported. It should be supplied on create")
		}
	}

	// check if hostConfig is in line with the current system settings.
	// It may happen cgroups are umounted or the like.
	//检查容器的启动配置是否合法。
	if _, err = daemon.verifyContainerSettings(container.HostConfig, nil, false); err != nil {
		return err
	}
	// Adapt for old containers in case we have updates in this function and
	// old containers never have chance to call the new function in create stage.
	//匹配旧版本的容器。
	if err := daemon.adaptContainerSettings(container.HostConfig, false); err != nil {
		return err
	}

	//真正启动容器的方法，请参考containerStart()方法。
	return daemon.containerStart(container)
}

// Start starts a container
func (daemon *Daemon) Start(container *container.Container) error {
	return daemon.containerStart(container)
}

// containerStart prepares the container to run by setting up everything the
// container needs, such as storage and networking, as well as links
// between containers. The container is left waiting for a signal to
// begin running.
//容器启动的核心方法。
func (daemon *Daemon) containerStart(container *container.Container) (err error) {

	//这里的锁是干什么的？
	container.Lock()
	defer container.Unlock()

	if container.Running {
		return nil
	}

	if container.RemovalInProgress || container.Dead {
		return fmt.Errorf("Container is marked for removal and cannot be started.")
	}

	// if we encounter an error during start we need to ensure that any other
	// setup has been cleaned up properly
	defer func() {
		if err != nil {
			container.SetError(err)
			// if no one else has set it, make sure we don't leave it at zero
			if container.ExitCode == 0 {
				container.ExitCode = 128
			}
			container.ToDisk()
			daemon.Cleanup(container)

			attributes := map[string]string{
				"exitCode": fmt.Sprintf("%d", container.ExitCode),
			}
			daemon.LogContainerEventWithAttributes(container, "die", attributes)
		}
	}()

	//挂载容器的文件系统。会调用daemon/daemon.go中的Mount()方法。
	//不过那个方法比较奇怪，感觉正常情况下并不会做什么事情。

	if err := daemon.conditionalMountOnStart(container); err != nil {
		return err
	}

	// Make sure NetworkMode has an acceptable value. We do this to ensure
	// backwards API compatibility.
	//设置默认的网络模式。
	container.HostConfig = runconfig.SetDefaultNetModeIfBlank(container.HostConfig)

	/*
		nitializeNetworking() 对网络进行初始化，docker网络模式有三种，分别是 bridge模式
		（每个容器用户单独的网络栈），host模式（与宿主机共用一个网络栈），contaier模式
		（与其他容器共用一个网络栈，猜测kubernate中的pod所用的模式）；
		根据config和hostConfig中的参数来确定容器的网络模式，然后调动libnetwork包来建立网络
	*/
	if err := daemon.initializeNetworking(container); err != nil {
		return err
	}

	//创建容器关于namespace和cgroup等的运行环境。在daemon/oci_linux.go中。
	//Spec中包括了容器的最基本的信息。
	spec, err := daemon.createSpec(container)
	if err != nil {
		return err
	}

	//运行容器，详细请见libcontainerd/client_linux.go中Create()方法。
	if err := daemon.containerd.Create(container.ID, *spec, libcontainerd.WithRestartManager(container.RestartManager(true))); err != nil {
		// if we receive an internal error from the initial start of a container then lets
		// return it instead of entering the restart loop
		// set to 127 for container cmd not found/does not exist)
		if strings.Contains(err.Error(), "executable file not found") ||
			strings.Contains(err.Error(), "no such file or directory") ||
			strings.Contains(err.Error(), "system cannot find the file specified") {
			container.ExitCode = 127
			err = fmt.Errorf("Container command '%s' not found or does not exist.", container.Path)
		}
		// set to 126 for container cmd can't be invoked errors
		if strings.Contains(err.Error(), syscall.EACCES.Error()) {
			container.ExitCode = 126
			err = fmt.Errorf("Container command '%s' could not be invoked.", container.Path)
		}

		container.Reset(false)

		// start event is logged even on error
		daemon.LogContainerEvent(container, "start")
		return err
	}

	return nil
}

// Cleanup releases any network resources allocated to the container along with any rules
// around how containers are linked together.  It also unmounts the container's root filesystem.
func (daemon *Daemon) Cleanup(container *container.Container) {
	daemon.releaseNetwork(container)

	container.UnmountIpcMounts(detachMounted)

	if err := daemon.conditionalUnmountOnCleanup(container); err != nil {
		// FIXME: remove once reference counting for graphdrivers has been refactored
		// Ensure that all the mounts are gone
		if mountid, err := daemon.layerStore.GetMountID(container.ID); err == nil {
			daemon.cleanupMountsByID(mountid)
		}
	}

	for _, eConfig := range container.ExecCommands.Commands() {
		daemon.unregisterExecCommand(container, eConfig)
	}

	if container.BaseFS != "" {
		if err := container.UnmountVolumes(false, daemon.LogVolumeEvent); err != nil {
			logrus.Warnf("%s cleanup: Failed to umount volumes: %v", container.ID, err)
		}
	}
	container.CancelAttachContext()
}
