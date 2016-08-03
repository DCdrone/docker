package daemon

import (
	"fmt"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/container"
	"github.com/docker/docker/image"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/idtools"
	"github.com/docker/docker/pkg/stringid"
	volumestore "github.com/docker/docker/volume/store"
	"github.com/docker/engine-api/types"
	containertypes "github.com/docker/engine-api/types/container"
	networktypes "github.com/docker/engine-api/types/network"
	"github.com/opencontainers/runc/libcontainer/label"
)

// ContainerCreate creates a container.
//这个就是那个backend(daemon)调用的ContainerCreate，他还会调用create（）
//create还会调用daemon.go中的NewContainer()
//让我们从这个函数入手，分析一下如何创建一个容器。
func (daemon *Daemon) ContainerCreate(params types.ContainerCreateConfig) (types.ContainerCreateResponse, error) {
	//这个函数几乎不做什么事情，主要是检查参数是否配置正确
	if params.Config == nil {
		return types.ContainerCreateResponse{}, fmt.Errorf("Config cannot be empty in order to create a container")
	}

	warnings, err := daemon.verifyContainerSettings(params.HostConfig, params.Config, false)
	if err != nil {
		return types.ContainerCreateResponse{Warnings: warnings}, err
	}

	err = daemon.verifyNetworkingConfig(params.NetworkingConfig)
	if err != nil {
		return types.ContainerCreateResponse{}, err
	}

	if params.HostConfig == nil {
		params.HostConfig = &containertypes.HostConfig{}
	}
	err = daemon.adaptContainerSettings(params.HostConfig, params.AdjustCPUShares)
	if err != nil {
		return types.ContainerCreateResponse{Warnings: warnings}, err
	}

	//调用create函数。
	container, err := daemon.create(params)
	if err != nil {
		return types.ContainerCreateResponse{Warnings: warnings}, daemon.imageNotExistToErrcode(err)
	}

	return types.ContainerCreateResponse{ID: container.ID, Warnings: warnings}, nil
}

// Create creates a new container from the given configuration with a given name.
func (daemon *Daemon) create(params types.ContainerCreateConfig) (retC *container.Container, retErr error) {
	var (
		container *container.Container
		img       *image.Image
		imgID     image.ID
		err       error
	)

	if params.Config.Image != "" {
		//获取镜像
		img, err = daemon.GetImage(params.Config.Image)
		if err != nil {
			return nil, err
		}
		//获取镜像ID号
		imgID = img.ID()
	}

	//合并并且检查参数
	if err := daemon.mergeAndVerifyConfig(params.Config, img); err != nil {
		return nil, err
	}

	//创建一个新的容器，此时需要跟踪分析newContainer方法，该方法在daemon/daemon.go中。
	//该方法返回的是container，是一个这样的结构：
	/*
		type Container struct {
			//这是通用的参数
			CommonContainer

			// 这个是平台特殊的参数，只在类unix系统上有意义。
			AppArmorProfile string
			HostnamePath    string
			HostsPath       string
			ShmPath         string
			ResolvConfPath  string
			SeccompProfile  string
			NoNewPrivileges bool
		}
	*/
	//创建容器实例，实际上只是在代码中创建，并没有创建文件系统和namespace。
	//实际上，创建容器的过程中最多也就创建到文件系统，namespace只有在容器
	//运行时候才会生效。
	if container, err = daemon.newContainer(params.Name, params.Config, imgID); err != nil {
		return nil, err
	}

	//如果创建容器出错，就试图删除容器。
	defer func() {
		if retErr != nil {
			if err := daemon.ContainerRm(container.ID, &types.ContainerRmConfig{ForceRemove: true}); err != nil {
				logrus.Errorf("Clean up Error! Cannot destroy container %s: %v", container.ID, err)
			}
		}
	}()

	//配置容器的安全设置，例如apparmor,selinux等。
	if err := daemon.setSecurityOptions(container, params.HostConfig); err != nil {
		return nil, err
	}

	// Set RWLayer for container after mount labels have been set
	// 设置可读写层，就是获取layID等信息,包括镜像层、容器层。
	//详情请见setRWLayer函数，就在本文件中。
	if err := daemon.setRWLayer(container); err != nil {
		return nil, err
	}

	//向daemon注册容器：
	//daemon.containers.Add(c.ID, c)
	if err := daemon.Register(container); err != nil {
		return nil, err
	}
	rootUID, rootGID, err := idtools.GetRootUIDGID(daemon.uidMaps, daemon.gidMaps)
	if err != nil {
		return nil, err
	}
	//设置容器root目录（元数据目录/var/lib/docker/containers）的权限。
	if err := idtools.MkdirAs(container.Root, 0700, rootUID, rootGID); err != nil {
		return nil, err
	}

	//这个方法主要做两件事情：
	//挂载volumes;
	//设置容器之间的链接links；
	if err := daemon.setHostConfig(container, params.HostConfig); err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil {
			if err := daemon.removeMountPoints(container, true); err != nil {
				logrus.Error(err)
			}
		}
	}()

	//这一步是处理和平台相关的操作，具体在daemon/create_unix.go文件中
	/*
			func (daemon *Daemon) createContainerPlatformSpecificSettings(container *container.Container, config *containertypes.Config, hostConfig *containertypes.HostConfig) error {
			if err := daemon.Mount(container); err != nil {
				return err
			}
			defer daemon.Unmount(container)

			rootUID, rootGID := daemon.GetRemappedUIDGID()
			if err := container.SetupWorkingDirectory(rootUID, rootGID); err != nil {
				return err
			}

			for spec := range config.Volumes {
				name := stringid.GenerateNonCryptoID()
				destination := filepath.Clean(spec)

				// Skip volumes for which we already have something mounted on that
				// destination because of a --volume-from.
				if container.IsDestinationMounted(destination) {
					continue
				}
				path, err := container.GetResourcePath(destination)
				if err != nil {
					return err
				}

				stat, err := os.Stat(path)
				if err == nil && !stat.IsDir() {
					return fmt.Errorf("cannot mount volume over existing file, file exists %s", path)
				}

				v, err := daemon.volumes.CreateWithRef(name, hostConfig.VolumeDriver, container.ID, nil, nil)
				if err != nil {
					return err
				}

				if err := label.Relabel(v.Path(), container.MountLabel, true); err != nil {
					return err
				}

				container.AddMountPointWithVolume(destination, v, true)
			}
			return daemon.populateVolumes(container)
		}
	*/
	//在这一步骤里面进行一些跟平台相关的设置，主要为mount目录文件，以及volume挂载。
	if err := daemon.createContainerPlatformSpecificSettings(container, params.Config, params.HostConfig); err != nil {
		return nil, err
	}

	//网络endpoints的配置
	var endpointsConfigs map[string]*networktypes.EndpointSettings
	if params.NetworkingConfig != nil {
		endpointsConfigs = params.NetworkingConfig.EndpointsConfig
	}

	//更新网路配置，在daemon/container_operations.go中，调用了libnetwork模块，需要仔细研究。
	//这里仅仅是更新container.NetworkSettings.Networks[]数组中的网络模式。并没有真正创建网络。
	if err := daemon.updateContainerNetworkSettings(container, endpointsConfigs); err != nil {
		return nil, err
	}

	//将容器的配置保存到磁盘。但是这个和另外一个todisk的区别是什么？
	if err := container.ToDiskLocking(); err != nil {
		logrus.Errorf("Error saving new container to disk: %v", err)
		return nil, err
	}

	//记录容器的事件日志。
	daemon.LogContainerEvent(container, "create")
	return container, nil
}

func (daemon *Daemon) generateSecurityOpt(ipcMode containertypes.IpcMode, pidMode containertypes.PidMode) ([]string, error) {
	if ipcMode.IsHost() || pidMode.IsHost() {
		return label.DisableSecOpt(), nil
	}
	if ipcContainer := ipcMode.Container(); ipcContainer != "" {
		c, err := daemon.GetContainer(ipcContainer)
		if err != nil {
			return nil, err
		}

		return label.DupSecOpt(c.ProcessLabel), nil
	}
	return nil, nil
}

func (daemon *Daemon) setRWLayer(container *container.Container) error {
	var layerID layer.ChainID
	if container.ImageID != "" {
		img, err := daemon.imageStore.Get(container.ImageID)
		if err != nil {
			return err
		}
		//获取镜像层
		layerID = img.RootFS.ChainID()
	}
	//通过镜像层ID，容器ID，MountLabel以及初始化层（例如/dev/pts,/proc,/sys,/etc/hosts等目录）创建可读写层
	//关于setupInitLayer中的内容可以参考daemon/daemon_unix.go
	rwLayer, err := daemon.layerStore.CreateRWLayer(container.ID, layerID, container.MountLabel, daemon.setupInitLayer)
	if err != nil {
		return err
	}
	container.RWLayer = rwLayer

	return nil
}

// VolumeCreate creates a volume with the specified name, driver, and opts
// This is called directly from the remote API
func (daemon *Daemon) VolumeCreate(name, driverName string, opts, labels map[string]string) (*types.Volume, error) {
	if name == "" {
		name = stringid.GenerateNonCryptoID()
	}

	v, err := daemon.volumes.Create(name, driverName, opts, labels)
	if err != nil {
		if volumestore.IsNameConflict(err) {
			return nil, fmt.Errorf("A volume named %s already exists. Choose a different volume name.", name)
		}
		return nil, err
	}

	daemon.LogVolumeEvent(v.Name(), "create", map[string]string{"driver": v.DriverName()})
	return volumeToAPIType(v), nil
}
