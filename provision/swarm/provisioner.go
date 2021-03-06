// Copyright 2016 tsuru authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package swarm

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/url"
	"strings"

	"github.com/docker/docker/api/types/swarm"
	"github.com/fsouza/go-dockerclient"
	"github.com/pkg/errors"
	"github.com/tsuru/config"
	"github.com/tsuru/tsuru/app"
	"github.com/tsuru/tsuru/app/image"
	tsuruErrors "github.com/tsuru/tsuru/errors"
	"github.com/tsuru/tsuru/event"
	tsuruNet "github.com/tsuru/tsuru/net"
	"github.com/tsuru/tsuru/provision"
	"github.com/tsuru/tsuru/provision/dockercommon"
	"github.com/tsuru/tsuru/provision/nodecontainer"
	"github.com/tsuru/tsuru/provision/servicecommon"
)

const (
	provisionerName = "swarm"
)

var swarmConfig swarmProvisionerConfig

type swarmProvisioner struct{}

var (
	_ provision.Provisioner              = &swarmProvisioner{}
	_ provision.ArchiveDeployer          = &swarmProvisioner{}
	_ provision.UploadDeployer           = &swarmProvisioner{}
	_ provision.ImageDeployer            = &swarmProvisioner{}
	_ provision.ShellProvisioner         = &swarmProvisioner{}
	_ provision.ExecutableProvisioner    = &swarmProvisioner{}
	_ provision.MessageProvisioner       = &swarmProvisioner{}
	_ provision.InitializableProvisioner = &swarmProvisioner{}
	_ provision.NodeProvisioner          = &swarmProvisioner{}
	_ provision.NodeContainerProvisioner = &swarmProvisioner{}
	_ provision.SleepableProvisioner     = &swarmProvisioner{}
	// _ provision.RollbackableDeployer     = &swarmProvisioner{}
	// _ provision.RebuildableDeployer      = &swarmProvisioner{}
	// _ provision.OptionalLogsProvisioner  = &swarmProvisioner{}
	// _ provision.UnitStatusProvisioner    = &swarmProvisioner{}
	// _ provision.NodeRebalanceProvisioner = &swarmProvisioner{}
	// _ provision.AppFilterProvisioner     = &swarmProvisioner{}
	// _ provision.ExtensibleProvisioner    = &swarmProvisioner{}
)

type swarmProvisionerConfig struct {
	swarmPort int
}

func init() {
	provision.Register(provisionerName, func() (provision.Provisioner, error) {
		return &swarmProvisioner{}, nil
	})
}

func (p *swarmProvisioner) Initialize() error {
	var err error
	swarmConfig.swarmPort, err = config.GetInt("swarm:swarm-port")
	if err != nil {
		swarmConfig.swarmPort = 2377
	}
	return nil
}

func (p *swarmProvisioner) GetName() string {
	return provisionerName
}

func (p *swarmProvisioner) Provision(a provision.App) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	_, err = client.CreateNetwork(docker.CreateNetworkOptions{
		Name:           networkNameForApp(a),
		Driver:         "overlay",
		CheckDuplicate: true,
		IPAM: docker.IPAMOptions{
			Driver: "default",
		},
	})
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (p *swarmProvisioner) Destroy(a provision.App) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	multiErrors := tsuruErrors.NewMultiError()
	processes, err := image.AllAppProcesses(a.GetName())
	if err != nil {
		multiErrors.Add(err)
	}
	for _, p := range processes {
		name := serviceNameForApp(a, p)
		err = client.RemoveService(docker.RemoveServiceOptions{
			ID: name,
		})
		if err != nil {
			if _, notFound := err.(*docker.NoSuchService); !notFound {
				multiErrors.Add(errors.WithStack(err))
			}
		}
	}
	err = client.RemoveNetwork(networkNameForApp(a))
	if err != nil {
		multiErrors.Add(errors.WithStack(err))
	}
	if multiErrors.Len() > 0 {
		return multiErrors
	}
	return nil
}

func (p *swarmProvisioner) AddUnits(a provision.App, units uint, processName string, w io.Writer) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	return servicecommon.ChangeUnits(&serviceManager{
		client: client,
	}, a, int(units), processName)
}

func (p *swarmProvisioner) RemoveUnits(a provision.App, units uint, processName string, w io.Writer) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	return servicecommon.ChangeUnits(&serviceManager{
		client: client,
	}, a, -int(units), processName)
}

func (p *swarmProvisioner) Restart(a provision.App, process string, w io.Writer) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	return servicecommon.ChangeAppState(&serviceManager{
		client: client,
	}, a, process, servicecommon.ProcessState{Start: true, Restart: true})
}

func (p *swarmProvisioner) Start(a provision.App, process string) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	return servicecommon.ChangeAppState(&serviceManager{
		client: client,
	}, a, process, servicecommon.ProcessState{Start: true})
}

func (p *swarmProvisioner) Stop(a provision.App, process string) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	return servicecommon.ChangeAppState(&serviceManager{
		client: client,
	}, a, process, servicecommon.ProcessState{Stop: true})
}

var stateMap = map[swarm.TaskState]provision.Status{
	swarm.TaskStateNew:       provision.StatusCreated,
	swarm.TaskStateAllocated: provision.StatusStarting,
	swarm.TaskStatePending:   provision.StatusStarting,
	swarm.TaskStateAssigned:  provision.StatusStarting,
	swarm.TaskStateAccepted:  provision.StatusStarting,
	swarm.TaskStatePreparing: provision.StatusStarting,
	swarm.TaskStateReady:     provision.StatusStarting,
	swarm.TaskStateStarting:  provision.StatusStarting,
	swarm.TaskStateRunning:   provision.StatusStarted,
	swarm.TaskStateComplete:  provision.StatusStopped,
	swarm.TaskStateShutdown:  provision.StatusStopped,
	swarm.TaskStateFailed:    provision.StatusError,
	swarm.TaskStateRejected:  provision.StatusError,
}

func taskToUnit(task *swarm.Task, service *swarm.Service, node *swarm.Node, a provision.App) provision.Unit {
	nodeLabels := provision.LabelSet{Labels: node.Spec.Labels, Prefix: tsuruLabelPrefix}
	host := tsuruNet.URLToHost(nodeLabels.NodeAddr())
	labels := provision.LabelSet{Labels: service.Spec.Annotations.Labels, Prefix: tsuruLabelPrefix}
	return provision.Unit{
		ID:          task.ID,
		AppName:     a.GetName(),
		ProcessName: labels.AppProcess(),
		Type:        a.GetPlatform(),
		Ip:          host,
		Status:      stateMap[task.Status.State],
		Address:     &url.URL{},
	}
}

func tasksToUnits(client *docker.Client, tasks []swarm.Task) ([]provision.Unit, error) {
	nodeMap := map[string]*swarm.Node{}
	serviceMap := map[string]*swarm.Service{}
	appsMap := map[string]provision.App{}
	units := []provision.Unit{}
	for _, t := range tasks {
		labels := provision.LabelSet{Labels: t.Spec.ContainerSpec.Labels, Prefix: tsuruLabelPrefix}
		if !labels.IsService() {
			continue
		}
		if t.DesiredState == swarm.TaskStateShutdown ||
			t.NodeID == "" ||
			t.ServiceID == "" {
			continue
		}
		if _, ok := nodeMap[t.NodeID]; !ok {
			node, err := client.InspectNode(t.NodeID)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			nodeMap[t.NodeID] = node
		}
		if _, ok := serviceMap[t.ServiceID]; !ok {
			service, err := client.InspectService(t.ServiceID)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			serviceMap[t.ServiceID] = service
		}
		service := serviceMap[t.ServiceID]
		serviceLabels := provision.LabelSet{Labels: service.Spec.Annotations.Labels, Prefix: tsuruLabelPrefix}
		appName := serviceLabels.AppName()
		if _, ok := appsMap[appName]; !ok {
			a, err := app.GetByName(appName)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			appsMap[appName] = a
		}
		units = append(units, taskToUnit(&t, serviceMap[t.ServiceID], nodeMap[t.NodeID], appsMap[appName]))
	}
	return units, nil
}

func (p *swarmProvisioner) Units(app provision.App) ([]provision.Unit, error) {
	client, err := chooseDBSwarmNode()
	if err != nil {
		if errors.Cause(err) == errNoSwarmNode {
			return []provision.Unit{}, nil
		}
		return nil, err
	}
	l, err := provision.ProcessLabels(provision.ProcessLabelsOpts{App: app, Prefix: tsuruLabelPrefix})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	tasks, err := client.ListTasks(docker.ListTasksOptions{
		Filters: map[string][]string{
			"label": toLabelSelectors(l.ToAppSelector()),
		},
	})
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return tasksToUnits(client, tasks)
}

func (p *swarmProvisioner) RoutableAddresses(a provision.App) ([]url.URL, error) {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return nil, err
	}
	imgID, err := image.AppCurrentImageName(a.GetName())
	if err != nil {
		if err != image.ErrNoImagesAvailable {
			return nil, err
		}
		return nil, nil
	}
	webProcessName, err := image.GetImageWebProcessName(imgID)
	if err != nil {
		return nil, err
	}
	if webProcessName == "" {
		return nil, nil
	}
	srvName := serviceNameForApp(a, webProcessName)
	srv, err := client.InspectService(srvName)
	if err != nil {
		return nil, err
	}
	var pubPort uint32
	if len(srv.Endpoint.Ports) > 0 {
		pubPort = srv.Endpoint.Ports[0].PublishedPort
	}
	if pubPort == 0 {
		return nil, nil
	}
	nodes, err := listValidNodes(client)
	if err != nil {
		return nil, err
	}
	for i := len(nodes) - 1; i >= 0; i-- {
		l := provision.LabelSet{Labels: nodes[i].Spec.Annotations.Labels, Prefix: tsuruLabelPrefix}
		if l.NodePool() != a.GetPool() {
			nodes[i], nodes[len(nodes)-1] = nodes[len(nodes)-1], nodes[i]
			nodes = nodes[:len(nodes)-1]
		}
	}
	addrs := make([]url.URL, len(nodes))
	for i, n := range nodes {
		l := provision.LabelSet{Labels: n.Spec.Labels, Prefix: tsuruLabelPrefix}
		host := tsuruNet.URLToHost(l.NodeAddr())
		addrs[i] = url.URL{
			Scheme: "http",
			Host:   fmt.Sprintf("%s:%d", host, pubPort),
		}
	}
	return addrs, nil
}

func bindUnit(a provision.App, unit *provision.Unit) error {
	err := a.BindUnit(unit)
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func findTaskByContainerId(tasks []swarm.Task, unitId string) (*swarm.Task, error) {
	for i, t := range tasks {
		if strings.HasPrefix(t.Status.ContainerStatus.ContainerID, unitId) {
			return &tasks[i], nil
		}
	}
	return nil, &provision.UnitNotFoundError{ID: unitId}
}

func (p *swarmProvisioner) RegisterUnit(a provision.App, unitId string, customData map[string]interface{}) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	l, err := provision.ProcessLabels(provision.ProcessLabelsOpts{App: a, Prefix: tsuruLabelPrefix})
	if err != nil {
		return errors.WithStack(err)
	}
	tasks, err := client.ListTasks(docker.ListTasksOptions{
		Filters: map[string][]string{
			"label": toLabelSelectors(l.ToAppSelector()),
		},
	})
	if err != nil {
		return err
	}
	task, err := findTaskByContainerId(tasks, unitId)
	if err != nil {
		return err
	}
	units, err := tasksToUnits(client, []swarm.Task{*task})
	if err != nil {
		return err
	}
	err = bindUnit(a, &units[0])
	if err != nil {
		return err
	}
	if customData == nil {
		return nil
	}
	labels := provision.LabelSet{Labels: task.Spec.ContainerSpec.Labels, Prefix: tsuruLabelPrefix}
	if !labels.IsDeploy() {
		return nil
	}
	buildingImage := labels.BuildImage()
	if buildingImage == "" {
		return errors.Errorf("invalid build image label for build task: %#v", task)
	}
	return image.SaveImageCustomData(buildingImage, customData)
}

func (p *swarmProvisioner) ListNodes(addressFilter []string) ([]provision.Node, error) {
	client, err := chooseDBSwarmNode()
	if err != nil {
		if errors.Cause(err) == errNoSwarmNode {
			return nil, nil
		}
		return nil, err
	}
	nodes, err := listValidNodes(client)
	if err != nil {
		return nil, err
	}
	var filterMap map[string]struct{}
	if len(addressFilter) > 0 {
		filterMap = map[string]struct{}{}
		for _, addr := range addressFilter {
			filterMap[addr] = struct{}{}
		}
	}
	nodeList := make([]provision.Node, 0, len(nodes))
	for i := range nodes {
		wrapped := &swarmNodeWrapper{Node: &nodes[i], provisioner: p}
		toAdd := true
		if filterMap != nil {
			_, toAdd = filterMap[wrapped.Address()]
		}
		if toAdd {
			nodeList = append(nodeList, wrapped)
		}
	}
	return nodeList, nil
}

func (p *swarmProvisioner) GetNode(address string) (provision.Node, error) {
	nodes, err := p.ListNodes([]string{address})
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, provision.ErrNodeNotFound
	}
	return nodes[0], nil
}

func (p *swarmProvisioner) NodeForNodeData(nodeData provision.NodeStatusData) (provision.Node, error) {
	client, err := chooseDBSwarmNode()
	if err != nil {
		if errors.Cause(err) == errNoSwarmNode {
			return nil, provision.ErrNodeNotFound
		}
	}
	tasks, err := client.ListTasks(docker.ListTasksOptions{})
	if err != nil {
		return nil, err
	}
	var task *swarm.Task
	for _, unitData := range nodeData.Units {
		task, err = findTaskByContainerId(tasks, unitData.ID)
		if err == nil {
			break
		}
		if _, isNotFound := errors.Cause(err).(*provision.UnitNotFoundError); !isNotFound {
			return nil, err
		}
	}
	if task != nil {
		node, err := client.InspectNode(task.NodeID)
		if err != nil {
			if _, notFound := err.(*docker.NoSuchNode); notFound {
				return nil, provision.ErrNodeNotFound
			}
			return nil, err
		}
		return &swarmNodeWrapper{Node: node, provisioner: p}, nil
	}
	return provision.FindNodeByAddrs(p, nodeData.Addrs)
}

func (p *swarmProvisioner) AddNode(opts provision.AddNodeOptions) error {
	init := false
	existingClient, err := chooseDBSwarmNode()
	if err != nil && errors.Cause(err) != errNoSwarmNode {
		return err
	}
	err = addNodeCredentials(opts)
	if err != nil {
		return err
	}
	newClient, err := newClient(opts.Address)
	if err != nil {
		return err
	}
	err = dockercommon.WaitDocker(newClient)
	if err != nil {
		return err
	}
	if existingClient == nil {
		err = initSwarm(newClient, opts.Address)
		existingClient = newClient
		init = true
	} else {
		err = joinSwarm(existingClient, newClient, opts.Address)
	}
	if err != nil {
		return err
	}
	dockerInfo, err := newClient.Info()
	if err != nil {
		return errors.WithStack(err)
	}
	nodeData, err := existingClient.InspectNode(dockerInfo.Swarm.NodeID)
	if err != nil {
		return errors.WithStack(err)
	}
	nodeData.Spec.Annotations.Labels = provision.NodeLabels(provision.NodeLabelsOpts{
		Addr:         opts.Address,
		CustomLabels: opts.Metadata,
	}).ToLabels()
	err = existingClient.UpdateNode(dockerInfo.Swarm.NodeID, docker.UpdateNodeOptions{
		Version:  nodeData.Version.Index,
		NodeSpec: nodeData.Spec,
	})
	if err != nil {
		return errors.WithStack(err)
	}
	err = updateDBSwarmNodes(existingClient)
	if err != nil {
		return err
	}
	if init {
		m := nodeContainerManager{client: existingClient}
		return servicecommon.EnsureNodeContainersCreated(&m, ioutil.Discard)
	}
	return nil
}

func (p *swarmProvisioner) RemoveNode(opts provision.RemoveNodeOptions) error {
	node, err := p.GetNode(opts.Address)
	if err != nil {
		return err
	}
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	swarmNode := node.(*swarmNodeWrapper).Node
	if opts.Rebalance {
		swarmNode.Spec.Availability = swarm.NodeAvailabilityDrain
		err = client.UpdateNode(swarmNode.ID, docker.UpdateNodeOptions{
			NodeSpec: swarmNode.Spec,
			Version:  swarmNode.Version.Index,
		})
		if err != nil {
			return errors.WithStack(err)
		}
	}
	nodes, err := listValidNodes(client)
	if err != nil {
		return errors.WithStack(err)
	}
	if len(nodes) == 1 {
		err = client.LeaveSwarm(docker.LeaveSwarmOptions{Force: true})
		if err != nil {
			return errors.WithStack(err)
		}
		return removeDBSwarmNodes()
	}
	err = client.RemoveNode(docker.RemoveNodeOptions{
		ID:    swarmNode.ID,
		Force: true,
	})
	if err != nil {
		return errors.WithStack(err)
	}
	return updateDBSwarmNodes(client)
}

func (p *swarmProvisioner) UpdateNode(opts provision.UpdateNodeOptions) error {
	node, err := p.GetNode(opts.Address)
	if err != nil {
		return err
	}
	swarmNode := node.(*swarmNodeWrapper).Node
	if opts.Disable {
		swarmNode.Spec.Availability = swarm.NodeAvailabilityPause
	} else if opts.Enable {
		swarmNode.Spec.Availability = swarm.NodeAvailabilityActive
	}
	for k, v := range opts.Metadata {
		if v == "" {
			delete(swarmNode.Spec.Annotations.Labels, k)
		} else {
			swarmNode.Spec.Annotations.Labels[k] = v
		}
	}
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	err = client.UpdateNode(swarmNode.ID, docker.UpdateNodeOptions{
		NodeSpec: swarmNode.Spec,
		Version:  swarmNode.Version.Index,
	})
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (p *swarmProvisioner) ArchiveDeploy(a provision.App, archiveURL string, evt *event.Event) (imgID string, err error) {
	baseImage := image.GetBuildImage(a)
	buildingImage, err := image.AppNewImageName(a.GetName())
	if err != nil {
		return "", errors.WithStack(err)
	}
	client, err := chooseDBSwarmNode()
	if err != nil {
		return "", err
	}
	cmds := dockercommon.ArchiveDeployCmds(a, archiveURL)
	srvID, task, err := runOnceBuildCmds(client, a, cmds, baseImage, buildingImage, evt)
	if srvID != "" {
		defer removeServiceAndLog(client, srvID)
	}
	if err != nil {
		return "", err
	}
	_, err = commitPushBuildImage(client, buildingImage, task.Status.ContainerStatus.ContainerID, a)
	if err != nil {
		return "", err
	}
	err = deployProcesses(a, buildingImage, nil)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return buildingImage, nil
}

func (p *swarmProvisioner) ImageDeploy(a provision.App, imgID string, evt *event.Event) (string, error) {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return "", err
	}
	if !strings.Contains(imgID, ":") {
		imgID = fmt.Sprintf("%s:latest", imgID)
	}
	fmt.Fprintln(evt, "---- Pulling image to tsuru ----")
	var buf bytes.Buffer
	cmds := []string{"/bin/bash", "-c", "cat /home/application/current/Procfile || cat /app/user/Procfile || cat /Procfile"}
	srvID, task, err := runOnceBuildCmds(client, a, cmds, imgID, "", &buf)
	if srvID != "" {
		defer removeServiceAndLog(client, srvID)
	}
	if err != nil {
		return "", err
	}
	client, err = clientForNode(client, task.NodeID)
	if err != nil {
		return "", err
	}
	newImage, err := dockercommon.PrepareImageForDeploy(dockercommon.PrepareImageArgs{
		Client:      client,
		App:         a,
		ProcfileRaw: buf.String(),
		ImageId:     imgID,
		Out:         evt,
	})
	if err != nil {
		return "", err
	}
	a.SetUpdatePlatform(true)
	err = deployProcesses(a, newImage, nil)
	if err != nil {
		return "", err
	}
	return newImage, nil
}

func (p *swarmProvisioner) UploadDeploy(app provision.App, archiveFile io.ReadCloser, fileSize int64, build bool, evt *event.Event) (string, error) {
	if build {
		return "", errors.New("running UploadDeploy with build=true is not yet supported")
	}
	tarFile := dockercommon.AddDeployTarFile(archiveFile, fileSize, "archive.tar.gz")
	defer tarFile.Close()
	buildingImage, err := p.buildImage(app, tarFile, evt)
	if err != nil {
		return "", err
	}
	err = deployProcesses(app, buildingImage, nil)
	if err != nil {
		return "", errors.WithStack(err)
	}
	return buildingImage, nil
}

func (p *swarmProvisioner) Shell(opts provision.ShellOptions) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	tasks, err := runningTasksForApp(client, opts.App, opts.Unit)
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		if opts.Unit != "" {
			return &provision.UnitNotFoundError{ID: opts.Unit}
		}
		return provision.ErrEmptyApp
	}
	nodeClient, err := clientForNode(client, tasks[0].NodeID)
	if err != nil {
		return err
	}
	cmds := []string{"/usr/bin/env", "TERM=" + opts.Term, "bash", "-l"}
	execCreateOpts := docker.CreateExecOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmds,
		Container:    tasks[0].Status.ContainerStatus.ContainerID,
		Tty:          true,
	}
	exec, err := nodeClient.CreateExec(execCreateOpts)
	if err != nil {
		return errors.WithStack(err)
	}
	startExecOptions := docker.StartExecOptions{
		InputStream:  opts.Conn,
		OutputStream: opts.Conn,
		ErrorStream:  opts.Conn,
		Tty:          true,
		RawTerminal:  true,
	}
	errs := make(chan error, 1)
	go func() {
		errs <- nodeClient.StartExec(exec.ID, startExecOptions)
	}()
	execInfo, err := nodeClient.InspectExec(exec.ID)
	for !execInfo.Running && err == nil {
		select {
		case startErr := <-errs:
			return startErr
		default:
			execInfo, err = nodeClient.InspectExec(exec.ID)
		}
	}
	if err != nil {
		return errors.WithStack(err)
	}
	nodeClient.ResizeExecTTY(exec.ID, opts.Height, opts.Width)
	return <-errs
}

func (p *swarmProvisioner) ExecuteCommand(stdout, stderr io.Writer, app provision.App, cmd string, args ...string) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	tasks, err := runningTasksForApp(client, app, "")
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return provision.ErrEmptyApp
	}
	for _, t := range tasks {
		err := execInTaskContainer(client, &t, stdout, stderr, cmd, args...)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *swarmProvisioner) ExecuteCommandOnce(stdout, stderr io.Writer, app provision.App, cmd string, args ...string) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	tasks, err := runningTasksForApp(client, app, "")
	if err != nil {
		return err
	}
	if len(tasks) == 0 {
		return provision.ErrEmptyApp
	}
	return execInTaskContainer(client, &tasks[0], stdout, stderr, cmd, args...)
}

func (p *swarmProvisioner) ExecuteCommandIsolated(stdout, stderr io.Writer, a provision.App, cmd string, args ...string) error {
	if a.GetDeploys() == 0 {
		return errors.New("commands can only be executed after the first deploy")
	}
	img, err := image.AppCurrentImageName(a.GetName())
	if err != nil {
		return err
	}
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	opts := tsuruServiceOpts{
		app:           a,
		image:         img,
		isIsolatedRun: true,
	}
	cmds := []string{"/bin/bash", "-lc", cmd}
	cmds = append(cmds, args...)
	serviceID, _, err := runOnceCmds(client, opts, cmds, stdout, stderr)
	if serviceID != "" {
		removeServiceAndLog(client, serviceID)
	}
	return err
}

type nodeContainerManager struct {
	client *docker.Client
}

func (m *nodeContainerManager) DeployNodeContainer(config *nodecontainer.NodeContainerConfig, pool string, filter servicecommon.PoolFilter, placementOnly bool) error {
	serviceSpec, err := serviceSpecForNodeContainer(config, pool, filter)
	if err != nil {
		return err
	}
	_, err = upsertService(serviceSpec, m.client, placementOnly)
	if err != nil {
		return err

	}
	return nil
}

func (p *swarmProvisioner) UpgradeNodeContainer(name string, pool string, writer io.Writer) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		if errors.Cause(err) == errNoSwarmNode {
			return nil
		}
		return err
	}
	m := nodeContainerManager{client: client}
	return servicecommon.UpgradeNodeContainer(&m, name, pool, writer)
}

func (p *swarmProvisioner) RemoveNodeContainer(name string, pool string, writer io.Writer) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		if errors.Cause(err) == errNoSwarmNode {
			return nil
		}
		return err
	}
	err = client.RemoveService(docker.RemoveServiceOptions{ID: nodeContainerServiceName(name, pool)})
	return errors.WithStack(err)
}

func (p *swarmProvisioner) StartupMessage() (string, error) {
	out := "Swarm provisioner reports the following nodes:\n"
	client, err := chooseDBSwarmNode()
	if err != nil {
		if errors.Cause(err) != errNoSwarmNode {
			return "", err
		}
		return out + "    No Swarm node available.\n", nil
	}
	nodeList, err := client.ListNodes(docker.ListNodesOptions{})
	if err != nil {
		return "", err
	}
	for _, node := range nodeList {
		l := provision.LabelSet{Labels: node.Spec.Labels, Prefix: tsuruLabelPrefix}
		out += fmt.Sprintf("    Swarm node: %s [%s] [%s]\n", l.NodeAddr(), node.Status.State, node.Spec.Role)
	}
	return out, nil
}

func deployProcesses(a provision.App, newImg string, updateSpec servicecommon.ProcessSpec) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	manager := &serviceManager{
		client: client,
	}
	return servicecommon.RunServicePipeline(manager, a, newImg, updateSpec)
}

type serviceManager struct {
	client *docker.Client
}

func (m *serviceManager) RemoveService(a provision.App, process string) error {
	srvName := serviceNameForApp(a, process)
	err := m.client.RemoveService(docker.RemoveServiceOptions{ID: srvName})
	if err != nil {
		return errors.WithStack(err)
	}
	return nil
}

func (m *serviceManager) DeployService(a provision.App, process string, pState servicecommon.ProcessState, imgID string) error {
	srvName := serviceNameForApp(a, process)
	srv, err := m.client.InspectService(srvName)
	if err != nil {
		if _, isNotFound := err.(*docker.NoSuchService); !isNotFound {
			return errors.WithStack(err)
		}
	}
	var baseSpec *swarm.ServiceSpec
	if srv != nil {
		baseSpec = &srv.Spec
	}
	spec, err := serviceSpecForApp(tsuruServiceOpts{
		app:          a,
		process:      process,
		image:        imgID,
		baseSpec:     baseSpec,
		processState: pState,
	})
	if err != nil {
		return err
	}
	if srv == nil {
		_, err = m.client.CreateService(docker.CreateServiceOptions{
			ServiceSpec: *spec,
		})
		if err != nil {
			return errors.WithStack(err)
		}
	} else {
		srv.Spec = *spec
		err = m.client.UpdateService(srv.ID, docker.UpdateServiceOptions{
			Version:     srv.Version.Index,
			ServiceSpec: srv.Spec,
		})
		if err != nil {
			return errors.WithStack(err)
		}
	}
	return nil
}

func runOnceBuildCmds(client *docker.Client, a provision.App, cmds []string, imgID, buildingImage string, w io.Writer) (string, *swarm.Task, error) {
	opts := tsuruServiceOpts{
		app:        a,
		image:      imgID,
		isDeploy:   true,
		buildImage: buildingImage,
	}
	return runOnceCmds(client, opts, cmds, w, w)
}

func runOnceCmds(client *docker.Client, opts tsuruServiceOpts, cmds []string, stdout, stderr io.Writer) (string, *swarm.Task, error) {
	spec, err := serviceSpecForApp(opts)
	if err != nil {
		return "", nil, err
	}
	spec.TaskTemplate.ContainerSpec.Command = cmds
	spec.TaskTemplate.RestartPolicy.Condition = swarm.RestartPolicyConditionNone
	srv, err := client.CreateService(docker.CreateServiceOptions{
		ServiceSpec: *spec,
	})
	if err != nil {
		return "", nil, errors.WithStack(err)
	}
	createdID := srv.ID
	tasks, err := waitForTasks(client, createdID, swarm.TaskStateShutdown)
	if err != nil {
		return createdID, nil, err
	}
	client, err = clientForNode(client, tasks[0].NodeID)
	if err != nil {
		return createdID, nil, err
	}
	contID := tasks[0].Status.ContainerStatus.ContainerID
	attachOpts := docker.AttachToContainerOptions{
		Container:    contID,
		OutputStream: stdout,
		ErrorStream:  stderr,
		Logs:         true,
		Stdout:       true,
		Stderr:       true,
		Stream:       true,
	}
	exitCode, err := safeAttachWaitContainer(client, attachOpts)
	if err != nil {
		return createdID, nil, err
	}
	if exitCode != 0 {
		return createdID, nil, errors.Errorf("unexpected result code for build container: %d", exitCode)
	}
	return createdID, &tasks[0], nil
}

func (p *swarmProvisioner) Sleep(a provision.App, process string) error {
	client, err := chooseDBSwarmNode()
	if err != nil {
		return err
	}
	return servicecommon.ChangeAppState(&serviceManager{
		client: client,
	}, a, process, servicecommon.ProcessState{Stop: true, Sleep: true})
}
