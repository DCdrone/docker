package libcontainerd

import (
	"encoding/json"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	containerd "github.com/docker/containerd/api/grpc/types"
	"github.com/docker/docker/restartmanager"
	"github.com/opencontainers/specs/specs-go"
	"golang.org/x/net/context"
)

type container struct {
	containerCommon

	// Platform specific fields are below here.
	pauseMonitor
	oom bool
}

func (ctr *container) clean() error {
	if os.Getenv("LIBCONTAINERD_NOCLEAN") == "1" {
		return nil
	}
	if _, err := os.Lstat(ctr.dir); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	if err := os.RemoveAll(ctr.dir); err != nil {
		return err
	}
	return nil
}

// cleanProcess removes the fifos used by an additional process.
// Caller needs to lock container ID before calling this method.
func (ctr *container) cleanProcess(id string) {
	if p, ok := ctr.processes[id]; ok {
		for _, i := range []int{syscall.Stdin, syscall.Stdout, syscall.Stderr} {
			if err := os.Remove(p.fifo(i)); err != nil {
				logrus.Warnf("failed to remove %v for process %v: %v", p.fifo(i), id, err)
			}
		}
	}
	delete(ctr.processes, id)
}

func (ctr *container) spec() (*specs.Spec, error) {
	var spec specs.Spec
	dt, err := ioutil.ReadFile(filepath.Join(ctr.dir, configFilename))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(dt, &spec); err != nil {
		return nil, err
	}
	return &spec, nil
}

func (ctr *container) start() error {
	//从文件中读取配置，准备启动容器。
	spec, err := ctr.spec()
	if err != nil {
		return nil
	}
	//打开IO流。
	iopipe, err := ctr.openFifos(spec.Process.Terminal)
	if err != nil {
		return err
	}

	//容器创建的请求。
	r := &containerd.CreateContainerRequest{
		Id:         ctr.containerID,
		BundlePath: ctr.dir,
		Stdin:      ctr.fifo(syscall.Stdin),
		Stdout:     ctr.fifo(syscall.Stdout),
		Stderr:     ctr.fifo(syscall.Stderr),
		// check to see if we are running in ramdisk to disable pivot root
		NoPivotRoot: os.Getenv("DOCKER_RAMDISK") != "",
	}
	//列表中增加容器
	ctr.client.appendContainer(ctr)

	//调用远程请求创建容器，这里为什么会有CS架构？
	//在D:\workspace_1\sourcedocker\vendor\src\github.com\docker\containerd\api\grpc\types\api.pb.go中
	/*
			func (c *aPIClient) CreateContainer(ctx context.Context, in *CreateContainerRequest, opts ...grpc.CallOption) (*CreateContainerResponse, error) {
			out := new(CreateContainerResponse)
			err := grpc.Invoke(ctx, "/types.API/CreateContainer", in, out, c.cc, opts...)
			if err != nil {
				return nil, err
			}
			return out, nil
		}
	*/
	//跟到这里怎么断了啊？这个有点麻烦了。
	//这里通过restapi协议调用containerd的api接口，由containerd调用containerd-shm再调用runC实现。
	resp, err := ctr.client.remote.apiClient.CreateContainer(context.Background(), r)
	if err != nil {
		ctr.closeFifos(iopipe)
		return err
	}
	ctr.startedAt = time.Now()

	if err := ctr.client.backend.AttachStreams(ctr.containerID, *iopipe); err != nil {
		return err
	}
	ctr.systemPid = systemPid(resp.Container)

	//更新容器状态。
	return ctr.client.backend.StateChanged(ctr.containerID, StateInfo{
		State: StateStart,
		Pid:   ctr.systemPid,
	})
}

func (ctr *container) newProcess(friendlyName string) *process {
	return &process{
		dir: ctr.dir,
		processCommon: processCommon{
			containerID:  ctr.containerID,
			friendlyName: friendlyName,
			client:       ctr.client,
		},
	}
}

func (ctr *container) handleEvent(e *containerd.Event) error {
	ctr.client.lock(ctr.containerID)
	defer ctr.client.unlock(ctr.containerID)
	switch e.Type {
	case StateExit, StatePause, StateResume, StateOOM:
		st := StateInfo{
			State:     e.Type,
			ExitCode:  e.Status,
			OOMKilled: e.Type == StateExit && ctr.oom,
		}
		if e.Type == StateOOM {
			ctr.oom = true
		}
		if e.Type == StateExit && e.Pid != InitFriendlyName {
			st.ProcessID = e.Pid
			st.State = StateExitProcess
		}
		if st.State == StateExit && ctr.restartManager != nil {
			restart, wait, err := ctr.restartManager.ShouldRestart(e.Status, false, time.Since(ctr.startedAt))
			if err != nil {
				logrus.Warnf("container %s %v", ctr.containerID, err)
			} else if restart {
				st.State = StateRestart
				ctr.restarting = true
				ctr.client.deleteContainer(e.Id)
				go func() {
					err := <-wait
					ctr.client.lock(ctr.containerID)
					defer ctr.client.unlock(ctr.containerID)
					ctr.restarting = false
					if err != nil {
						st.State = StateExit
						ctr.clean()
						ctr.client.q.append(e.Id, func() {
							if err := ctr.client.backend.StateChanged(e.Id, st); err != nil {
								logrus.Error(err)
							}
						})
						if err != restartmanager.ErrRestartCanceled {
							logrus.Error(err)
						}
					} else {
						ctr.start()
					}
				}()
			}
		}

		// Remove process from list if we have exited
		// We need to do so here in case the Message Handler decides to restart it.
		switch st.State {
		case StateExit:
			ctr.clean()
			ctr.client.deleteContainer(e.Id)
		case StateExitProcess:
			ctr.cleanProcess(st.ProcessID)
		}
		ctr.client.q.append(e.Id, func() {
			if err := ctr.client.backend.StateChanged(e.Id, st); err != nil {
				logrus.Error(err)
			}
			if e.Type == StatePause || e.Type == StateResume {
				ctr.pauseMonitor.handle(e.Type)
			}
			if e.Type == StateExit {
				if en := ctr.client.getExitNotifier(e.Id); en != nil {
					en.close()
				}
			}
		})

	default:
		logrus.Debugf("event unhandled: %+v", e)
	}
	return nil
}

// discardFifos attempts to fully read the container fifos to unblock processes
// that may be blocked on the writer side.
func (ctr *container) discardFifos() {
	for _, i := range []int{syscall.Stdout, syscall.Stderr} {
		f := ctr.fifo(i)
		c := make(chan struct{})
		go func() {
			close(c) // this channel is used to not close the writer too early, before readonly open has been called.
			io.Copy(ioutil.Discard, openReaderFromFifo(f))
		}()
		<-c
		closeReaderFifo(f) // avoid blocking permanently on open if there is no writer side
	}
}
