package daemon

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/logger"
	"github.com/docker/docker/libcontainerd"
	"github.com/docker/docker/runconfig"
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
		daemon.LogContainerEvent(c, "oom")
	case libcontainerd.StateExit:
		c.Lock()
		defer c.Unlock()
		c.Wait()

		// Save the LogDriver before calling Reset.  While c.Wait above will block
		// until all the copying from containerd into StreamConfig occurs (it
		// matches the s.Add/s.Done in AttachStreams below), only Reset will block
		// until these streams are copied into the LogDriver. Reset will set
		// c.LogDriver to nil, so we grab it first.
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

		// Now that we've copied everything into logDriver, send one last message.
		stopMessage := fmt.Sprintf(
			`{"type":"stop","exitCode":%d,"oomKilled":%v}`, e.ExitCode, e.OOMKilled)
		if err := logDriver.Log(&logger.Message{Line: []byte(stopMessage), Source: "event"}); err != nil {
			// At least the error will show up in journald without the appropriate tags...
			logrus.Errorf("Failed to send 'stop' event to logging driver: %v", err)
		}

		c.SetStopped(platformConstructExitStatus(e))
		attributes := map[string]string{
			"exitCode": strconv.Itoa(int(e.ExitCode)),
		}
		daemon.LogContainerEventWithAttributes(c, "die", attributes)
		daemon.Cleanup(c)
		// FIXME: here is race condition between two RUN instructions in Dockerfile
		// because they share same runconfig and change image. Must be fixed
		// in builder/builder.go
		return c.ToDisk()
	case libcontainerd.StateRestart:
		c.Lock()
		defer c.Unlock()
		c.Reset(false)
		c.RestartCount++
		c.SetRestarting(platformConstructExitStatus(e))
		attributes := map[string]string{
			"exitCode": strconv.Itoa(int(e.ExitCode)),
		}
		daemon.LogContainerEventWithAttributes(c, "die", attributes)
		return c.ToDisk()
	case libcontainerd.StateExitProcess:
		c.Lock()
		defer c.Unlock()
		if execConfig := c.ExecCommands.Get(e.ProcessID); execConfig != nil {
			ec := int(e.ExitCode)
			execConfig.ExitCode = &ec
			execConfig.Running = false
			execConfig.Wait()
			if err := execConfig.CloseStreams(); err != nil {
				logrus.Errorf("%s: %s", c.ID, err)
			}

			// remove the exec command from the container's store only and not the
			// daemon's store so that the exec command can be inspected.
			c.ExecCommands.Delete(execConfig.ID)
		} else {
			logrus.Warnf("Ignoring StateExitProcess for %v but no exec command found", e)
		}
	case libcontainerd.StateStart, libcontainerd.StateRestore:
		c.SetRunning(int(e.Pid), e.State == libcontainerd.StateStart)
		c.HasBeenManuallyStopped = false
		if err := c.ToDisk(); err != nil {
			c.Reset(false)
			return err
		}
		daemon.LogContainerEvent(c, "start")
	case libcontainerd.StatePause:
		c.Paused = true
		daemon.LogContainerEvent(c, "pause")
	case libcontainerd.StateResume:
		c.Paused = false
		daemon.LogContainerEvent(c, "unpause")
	}

	return nil
}

// AttachStreams is called by libcontainerd to connect the stdio.
func (daemon *Daemon) AttachStreams(id string, iop libcontainerd.IOPipe) error {
	var s *runconfig.StreamConfig
	c := daemon.containers.Get(id)
	if c == nil {
		ec, err := daemon.getExecConfig(id)
		if err != nil {
			return fmt.Errorf("no such exec/container: %s", id)
		}
		s = ec.StreamConfig
	} else {
		s = c.StreamConfig
		if err := daemon.StartLogging(c); err != nil {
			c.Reset(false)
			return err
		}
	}

	if stdin := s.Stdin(); stdin != nil {
		if iop.Stdin != nil {
			go func() {
				io.Copy(iop.Stdin, stdin)
				iop.Stdin.Close()
			}()
		}
	} else {
		if c != nil && !c.Config.Tty {
			// tty is enabled, so dont close containerd's iopipe stdin.
			if iop.Stdin != nil {
				iop.Stdin.Close()
			}
		}
	}

	copy := func(w io.Writer, r io.Reader) {
		s.Add(1)
		go func() {
			if _, err := io.Copy(w, r); err != nil {
				logrus.Errorf("%v stream copy error: %v", id, err)
			}
			s.Done()
		}()
	}

	if iop.Stdout != nil {
		copy(s.Stdout(), iop.Stdout)
	}
	if iop.Stderr != nil {
		copy(s.Stderr(), iop.Stderr)
	}

	return nil
}
