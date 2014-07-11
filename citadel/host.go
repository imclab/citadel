package main

import (
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/citadel/citadel"
	"github.com/citadel/citadel/repository"
	"github.com/citadel/citadel/utils"
	"github.com/codegangsta/cli"
	"github.com/samalba/dockerclient"
)

type (
	HostEngine struct {
		client     *dockerclient.DockerClient
		repository *repository.Repository
		id         string
		listenAddr string
	}
)

var hostCommand = cli.Command{
	Name:   "host",
	Usage:  "run the host and connect it to the cluster",
	Action: hostAction,
	Flags: []cli.Flag{
		cli.StringFlag{"host-id", "", "specify host id (default: detected)"},
		cli.StringFlag{"region", "", "region where the host is running"},
		cli.StringFlag{"addr", "", "external ip address for the host"},
		cli.StringFlag{"docker", "unix:///var/run/docker.sock", "docker remote ip address"},
		cli.IntFlag{"cpus", -1, "number of cpus available to the host"},
		cli.IntFlag{"memory", -1, "number of mb of memory available to the host"},
		cli.StringFlag{"listen, l", ":8787", "listen address"},
	},
}

func hostAction(context *cli.Context) {
	var (
		cpus       = context.Int("cpus")
		memory     = context.Int("memory")
		addr       = context.String("addr")
		region     = context.String("region")
		hostId     = context.String("host-id")
		listenAddr = context.String("listen")
	)
	if hostId == "" {
		id, err := utils.GetMachineID()
		if err != nil {
			logger.WithField("error", err).Fatal("unable to read machine id")
		}
		hostId = id
	}

	switch {
	case cpus < 1:
		logger.Fatal("cpus must have a value")
	case memory < 1:
		logger.Fatal("memory must have a value")
	case addr == "":
		logger.Fatal("addr must have a value")
	case region == "":
		logger.Fatal("region must have a value")
	}

	machines := strings.Split(context.GlobalString("etcd-machines"), ",")
	r := repository.New(machines, "citadel")

	host := &citadel.Host{
		ID:     hostId,
		Memory: memory,
		Cpus:   cpus,
		Addr:   addr,
		Region: region,
	}

	if err := r.SaveHost(host); err != nil {
		logger.WithField("error", err).Fatal("unable to save host")
	}
	defer r.DeleteHost(hostId)

	client, err := dockerclient.NewDockerClient(context.String("docker"))
	if err != nil {
		logger.WithField("error", err).Fatal("unable to connect to docker")
	}

	hostEngine := &HostEngine{
		client:     client,
		repository: r,
		id:         hostId,
		listenAddr: listenAddr,
	}
	// start
	go hostEngine.run()
	// watch for operations
	go hostEngine.watch()
	// handle stop signal
	hostEngine.waitForInterrupt()
}

func (eng *HostEngine) waitForInterrupt() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM, syscall.SIGQUIT)
	for _ = range sigChan {
		// stop engine
		eng.stop()
		os.Exit(0)
	}
}

func (eng *HostEngine) run() {
	logger.Info("Starting Citadel")
	if err := eng.loadContainers(); err != nil {
		logger.WithField("error", err).Fatal("unable to load containers")
	}

	// listen for events
	eng.client.StartMonitorEvents(eng.dockerEventHandler)

	if err := http.ListenAndServe(eng.listenAddr, nil); err != nil {
		logger.WithField("error", err).Fatal("unable to listen on http")
	}
}

func (eng *HostEngine) stop() {
	logger.Info("Stopping")
	// remove host from repository
	eng.repository.DeleteHost(eng.id)
}

func (eng *HostEngine) loadContainers() error {
	eng.repository.DeleteHostContainers(eng.id)

	containers, err := eng.client.ListContainers(true)
	if err != nil {
		return err
	}

	for _, c := range containers {
		cc, err := eng.generateContainerInfo(c)
		if err != nil {
			return err
		}
		if err := eng.repository.SaveContainer(cc); err != nil {
			return err
		}
	}

	return nil
}

func (eng *HostEngine) generateContainerInfo(cnt interface{}) (*citadel.Container, error) {
	c := cnt.(dockerclient.Container)
	info, err := eng.client.InspectContainer(c.Id)
	if err != nil {
		return nil, err
	}
	cc := &citadel.Container{
		ID:     info.Id,
		Image:  utils.CleanImageName(c.Image),
		HostID: eng.id,
		Cpus:   info.Config.CpuShares, // FIXME: not the right place, this is cpuset
	}

	if info.Config.Memory > 0 {
		cc.Memory = info.Config.Memory / 1024 / 1024
	}

	if info.State.Running {
		cc.State.Status = citadel.Running
	} else {
		cc.State.Status = citadel.Stopped
	}
	cc.State.ExitCode = info.State.ExitCode
	return cc, nil
}

func (eng *HostEngine) dockerEventHandler(event *dockerclient.Event, args ...interface{}) {
	switch event.Status {
	case "destroy":
		// remove container from repository
		if err := eng.repository.DeleteContainer(eng.id, event.Id); err != nil {
			logger.Warnf("Unable to remove container from repository: %s", err)
		}
	default:
		// reload containers into repository
		// when adding a single container, the Container struct is not
		// returned but instead ContainerInfo.  to keep the same
		// generateContainerInfo for a citadel container, i simply
		// re-run the loadContainers.  this can probably be improved.
		eng.loadContainers()
	}
}

func (eng *HostEngine) watch() {
	tickerChan := time.NewTicker(time.Millisecond * 2000).C
	for _ = range tickerChan {
		tasks, err := eng.repository.FetchTasks()
		if err != nil {
			logger.Fatal("unable to fetch queue: %s", err)
		}

		for _, task := range tasks {
			// filter this hosts tasks
			if task.Host == eng.id {
				go eng.taskHandler(task)
			}
		}
	}
}

func (eng *HostEngine) taskHandler(task *citadel.Task) {
	switch task.Command {
	case "run":
		logger.WithFields(logrus.Fields{
			"host": task.Host,
		}).Info("processing run task")

		eng.runHandler(task)
	case "restart":
		logger.WithFields(logrus.Fields{
			"host": task.Host,
		}).Info("processing restart task")

		eng.restartHandler(task)
	case "stop":
		logger.WithFields(logrus.Fields{
			"host": task.Host,
		}).Info("processing stop task")

		eng.stopHandler(task)
	case "destroy":
		logger.WithFields(logrus.Fields{
			"host": task.Host,
		}).Info("processing destroy task")

		eng.destroyHandler(task)
	default:
		logger.WithFields(logrus.Fields{
			"command": task.Command,
		}).Error("unknown task command")
	}
}

func (eng *HostEngine) runHandler(task *citadel.Task) {
	logger.WithFields(logrus.Fields{
		"host":      task.Host,
		"image":     task.Image,
		"cpus":      task.Cpus,
		"memory":    task.Memory,
		"instances": task.Instances,
	}).Info("running container")

	eng.repository.DeleteTask(task.ID)

	for i := 0; i < task.Instances; i++ {
		containerConfig := &dockerclient.ContainerConfig{
			Image:     task.Image,
			Memory:    task.Memory * 1024 * 1024,
			CpuShares: task.Cpus,
		}

		containerId, err := eng.client.CreateContainer(containerConfig, "")
		if err != nil {
			logger.WithFields(logrus.Fields{
				"err": err,
			}).Error("error creating container")
			return
		}

		if err := eng.client.StartContainer(containerId, nil); err != nil {
			logger.WithFields(logrus.Fields{
				"err": err,
			}).Error("error starting container")
			return
		}

		logger.WithFields(logrus.Fields{
			"host":  task.Host,
			"id":    containerId,
			"image": task.Image,
		}).Info("started container")
	}
}

func (eng *HostEngine) stopHandler(task *citadel.Task) {
	logger.WithFields(logrus.Fields{
		"host": task.Host,
		"id":   task.ContainerID,
	}).Info("stopping container")

	defer eng.repository.DeleteTask(task.ID)

	containerId := task.ContainerID
	if err := eng.client.StopContainer(containerId, 10); err != nil {
		logger.WithFields(logrus.Fields{
			"id":  containerId,
			"err": err,
		}).Error("error stopping container")
	}
}

func (eng *HostEngine) restartHandler(task *citadel.Task) {
	logger.WithFields(logrus.Fields{
		"host": task.Host,
		"id":   task.ContainerID,
	}).Info("restarting container")

	defer eng.repository.DeleteTask(task.ID)

	containerId := task.ContainerID
	if err := eng.client.RestartContainer(containerId, 10); err != nil {
		logger.WithFields(logrus.Fields{
			"containerId": containerId,
			"err":         err,
		}).Error("error restarting container")
	}
}

func (eng *HostEngine) destroyHandler(task *citadel.Task) {
	logger.WithFields(logrus.Fields{
		"host": task.Host,
		"id":   task.ContainerID,
	}).Info("destroying container")

	defer eng.repository.DeleteTask(task.ID)

	containerId := task.ContainerID
	if err := eng.client.KillContainer(containerId); err != nil {
		logger.WithFields(logrus.Fields{
			"containerId": containerId,
			"err":         err,
		}).Error("error killing container")
		return
	}

	if err := eng.client.RemoveContainer(containerId); err != nil {
		logger.WithFields(logrus.Fields{
			"containerId": containerId,
			"err":         err,
		}).Error("error removing container")
	}
}
