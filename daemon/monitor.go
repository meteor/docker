package daemon

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/libcontainerd"
	"github.com/docker/docker/restartmanager"
)

// StateChanged updates daemon state changes from containerd
func (daemon *Daemon) StateChanged(id string, e libcontainerd.StateInfo) error {
	c := daemon.containers.Get(id)
	if c == nil {
		return fmt.Errorf("no such container: %s", id)
	}

	switch e.State {
	case libcontainerd.StateOOM:
		// StateOOM is Linux specific and should never be hit on Windows
		if runtime.GOOS == "windows" {
			return errors.New("Received StateOOM from libcontainerd on Windows. This should never happen.")
		}
		daemon.updateHealthMonitor(c)
		daemon.LogContainerEvent(c, "oom")
	case libcontainerd.StateExit:
		// if container's AutoRemove flag is set, remove it after clean up
		autoRemove := func() {
			c.Lock()
			ar := c.HostConfig.AutoRemove
			c.Unlock()
			if ar {
				if err := daemon.ContainerRm(c.ID, &types.ContainerRmConfig{ForceRemove: true, RemoveVolume: true}); err != nil {
					logrus.Errorf("can't remove container %s: %v", c.ID, err)
				}
			}
		}

		c.Lock()
		c.StreamConfig.Wait()

		// Save the LogDriver before calling Reset.  While c.Wait above will block
		// until all the copying from containerd into StreamConfig occurs (it
		// matches the s.Add/s.Done in CopyToPipe called by InitializeStdio), only
		// Reset will block until these streams are copied into the LogDriver. Reset
		// will set c.LogDriver to nil, so we grab it first.
		//
		// It will also call c.LogDriver.Close(), but for journald, Close only
		// closes state related to reading from the journal (for `docker logs`,
		// etc), not writing to it, so it's OK for us to use it after Close. (It
		// also prints our final suppression message, if any.)
		//
		// It's also OK for us to grab this field because we've called c.Lock
		// above.
		logDriver := c.LogDriver

		c.Reset(false)

		if logDriver != nil {
			// Now that we've copied everything into logDriver, send one last message.
			stopMessage := fmt.Sprintf(
				`{"type":"stop","exitCode":%d,"oomKilled":%v}`, e.ExitCode, e.OOMKilled)
			if err := logDriver.Log(&logger.Message{Line: []byte(stopMessage), Source: "event"}); err != nil {
				// At least the error will show up in journald without the appropriate tags...
				logrus.Errorf("Failed to send 'stop' event to logging driver: %v", err)
			}
		}

		restart, wait, err := c.RestartManager().ShouldRestart(e.ExitCode, c.HasBeenManuallyStopped, time.Since(c.StartedAt))
		if err == nil && restart {
			c.RestartCount++
			c.SetRestarting(platformConstructExitStatus(e))
		} else {
			c.SetStopped(platformConstructExitStatus(e))
			defer autoRemove()
		}

		// cancel healthcheck here, they will be automatically
		// restarted if/when the container is started again
		daemon.stopHealthchecks(c)
		attributes := map[string]string{
			"exitCode": strconv.Itoa(int(e.ExitCode)),
		}
		daemon.LogContainerEventWithAttributes(c, "die", attributes)
		daemon.Cleanup(c)

		if err == nil && restart {
			go func() {
				err := <-wait
				if err == nil {
					if err = daemon.containerStart(c, "", "", false); err != nil {
						logrus.Debugf("failed to restart container: %+v", err)
					}
				}
				if err != nil {
					c.SetStopped(platformConstructExitStatus(e))
					defer autoRemove()
					if err != restartmanager.ErrRestartCanceled {
						logrus.Errorf("restartmanger wait error: %+v", err)
					}
				}
			}()
		}

		defer c.Unlock()
		if err := c.ToDisk(); err != nil {
			return err
		}
		return daemon.postRunProcessing(c, e)
	case libcontainerd.StateExitProcess:
		if execConfig := c.ExecCommands.Get(e.ProcessID); execConfig != nil {
			ec := int(e.ExitCode)
			execConfig.Lock()
			defer execConfig.Unlock()
			execConfig.ExitCode = &ec
			execConfig.Running = false
			execConfig.StreamConfig.Wait()
			if err := execConfig.CloseStreams(); err != nil {
				logrus.Errorf("failed to cleanup exec %s streams: %s", c.ID, err)
			}

			// remove the exec command from the container's store only and not the
			// daemon's store so that the exec command can be inspected.
			c.ExecCommands.Delete(execConfig.ID)
		} else {
			logrus.Warnf("Ignoring StateExitProcess for %v but no exec command found", e)
		}
	case libcontainerd.StateStart, libcontainerd.StateRestore:
		// Container is already locked in this case
		c.SetRunning(int(e.Pid), e.State == libcontainerd.StateStart)
		c.HasBeenManuallyStopped = false
		c.HasBeenStartedBefore = true
		if err := c.ToDisk(); err != nil {
			c.Reset(false)
			return err
		}
		daemon.initHealthMonitor(c)
		daemon.LogContainerEvent(c, "start")
	case libcontainerd.StatePause:
		// Container is already locked in this case
		c.Paused = true
		if err := c.ToDisk(); err != nil {
			return err
		}
		daemon.updateHealthMonitor(c)
		daemon.LogContainerEvent(c, "pause")
	case libcontainerd.StateResume:
		// Container is already locked in this case
		c.Paused = false
		if err := c.ToDisk(); err != nil {
			return err
		}
		daemon.updateHealthMonitor(c)
		daemon.LogContainerEvent(c, "unpause")
	}

	return nil
}
