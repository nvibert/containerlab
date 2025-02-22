// Copyright 2020 Nokia
// Licensed under the BSD 3-Clause License.
// SPDX-License-Identifier: BSD-3-Clause

package clab

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/srl-labs/containerlab/cert"
	"github.com/srl-labs/containerlab/clab/dependency_manager"
	errs "github.com/srl-labs/containerlab/errors"
	"github.com/srl-labs/containerlab/links"
	"github.com/srl-labs/containerlab/nodes"
	"github.com/srl-labs/containerlab/runtime"
	_ "github.com/srl-labs/containerlab/runtime/all"
	"github.com/srl-labs/containerlab/runtime/docker"
	"github.com/srl-labs/containerlab/runtime/ignite"
	"github.com/srl-labs/containerlab/types"
	"github.com/srl-labs/containerlab/utils"
	"golang.org/x/crypto/ssh"
	"golang.org/x/exp/slices"
)

type CLab struct {
	Config    *Config `json:"config,omitempty"`
	TopoPaths *types.TopoPaths
	Nodes     map[string]nodes.Node `json:"nodes,omitempty"`
	Links     map[int]links.Link    `json:"links,omitempty"`
	Endpoints []links.Endpoint
	Runtimes  map[string]runtime.ContainerRuntime `json:"runtimes,omitempty"`
	// reg is a registry of node kinds
	Reg  *nodes.NodeRegistry
	Cert *cert.Cert
	// List of SSH public keys extracted from the ~/.ssh/authorized_keys file
	// and ~/.ssh/*.pub files.
	// The keys are used to enable key-based SSH access for the nodes.
	SSHPubKeys []ssh.PublicKey

	m             *sync.RWMutex
	timeout       time.Duration
	globalRuntime string
	// nodeFilter is a list of node names to be deployed,
	// names are provided exactly as they are listed in the topology file.
	nodeFilter []string
}

type ClabOption func(c *CLab) error

func WithTimeout(dur time.Duration) ClabOption {
	return func(c *CLab) error {
		if dur <= 0 {
			return errors.New("zero or negative timeouts are not allowed")
		}
		c.timeout = dur
		return nil
	}
}

// WithDebug sets debug mode.
func WithDebug(debug bool) ClabOption {
	return func(c *CLab) error {
		c.Config.Debug = debug
		return nil
	}
}

// WithRuntime option sets a container runtime to be used by containerlab.
func WithRuntime(name string, rtconfig *runtime.RuntimeConfig) ClabOption {
	return func(c *CLab) error {
		name, rInit, err := RuntimeInitializer(name)
		if err != nil {
			return err
		}

		c.globalRuntime = name

		r := rInit()
		log.Debugf("Running runtime.Init with params %+v and %+v", rtconfig, c.Config.Mgmt)
		err = r.Init(
			runtime.WithConfig(rtconfig),
			runtime.WithMgmtNet(c.Config.Mgmt),
		)
		if err != nil {
			return fmt.Errorf("failed to init the container runtime: %v", err)
		}

		c.Runtimes[name] = r
		log.Debugf("initialized a runtime with params %+v", r)
		return nil
	}
}

// RuntimeInitializer returns a runtime initializer function for a provided runtime name.
func RuntimeInitializer(name string) (string, runtime.Initializer, error) {
	// define runtime name.
	// order of preference: cli flag -> env var -> default value of docker
	envN := os.Getenv("CLAB_RUNTIME")
	log.Debugf("env runtime var value is %v", envN)
	switch {
	case name != "":
	case envN != "":
		name = envN
	default:
		name = docker.RuntimeName
	}
	if rInit, ok := runtime.ContainerRuntimes[name]; ok {
		return name, rInit, nil
	}
	return name, nil, fmt.Errorf("unknown container runtime %q", name)
}

func WithKeepMgmtNet() ClabOption {
	return func(c *CLab) error {
		c.GlobalRuntime().WithKeepMgmtNet()
		return nil
	}
}

func WithTopoPath(path, varsFile string) ClabOption {
	return func(c *CLab) error {
		file, err := c.ProcessTopoPath(path)
		if err != nil {
			return err
		}
		if err := c.GetTopology(file, varsFile); err != nil {
			return fmt.Errorf("failed to read topology file: %v", err)
		}

		return c.initMgmtNetwork()
	}
}

// ProcessTopoPath takes a topology path, which might be the path to a directory or a file
// or stdin or a URL and returns the topology file name if found.
func (c *CLab) ProcessTopoPath(path string) (string, error) {
	var file string
	var err error

	switch {
	case path == "-" || path == "stdin":
		file, err = readFromStdin(c.TopoPaths.ClabTmpDir())
		if err != nil {
			return "", err
		}
	// if the path is not a local file and a URL, download the file and store it in the tmp dir
	case !utils.FileOrDirExists(path) && utils.IsHttpURL(path, true):
		file, err = downloadTopoFile(path, c.TopoPaths.ClabTmpDir())
		if err != nil {
			return "", err
		}

	case path == "":
		return "", fmt.Errorf("provide a path to the clab topology file")

	default:
		file, err = FindTopoFileByPath(path)
		if err != nil {
			return "", err
		}
	}
	return file, nil
}

// findTopoFileByPath takes a topology path, which might be the path to a directory
// and returns the topology file name if found.
func FindTopoFileByPath(path string) (string, error) {
	finfo, err := os.Stat(path)
	if err != nil {
		return "", err
	}

	// by default we assume the path points to a clab file
	file := path

	// we might have gotten a dirname
	// lets try to find a single *.clab.y*ml
	if finfo.IsDir() {
		matches, err := filepath.Glob(filepath.Join(path, "*.clab.y*ml"))
		if err != nil {
			return "", err
		}

		switch len(matches) {
		case 1:
			// single file found, using it
			file = matches[0]
		case 0:
			// no files found
			return "", fmt.Errorf("no topology files found in directory %q", path)
		default:
			// multiple files found
			var filenames []string
			// extract just filename -> no path
			for _, match := range matches {
				filenames = append(filenames, filepath.Base(match))
			}

			return "", fmt.Errorf("found multiple topology definitions [ %s ] in a given directory %q. Provide the specific filename", strings.Join(filenames, ", "), path)
		}
	}

	return file, nil
}

// readFromStdin reads the topology file from stdin
// creates a temp file with topology contents
// and returns a path to the temp file.
func readFromStdin(tempDir string) (string, error) {
	tmpFile, err := os.CreateTemp(tempDir, "topo-*.clab.yml")
	if err != nil {
		return "", err
	}

	_, err = tmpFile.ReadFrom(os.Stdin)
	if err != nil {
		return "", err
	}

	return tmpFile.Name(), nil
}

func downloadTopoFile(url, tempDir string) (string, error) {
	tmpFile, err := os.CreateTemp(tempDir, "topo-*.clab.yml")
	if err != nil {
		return "", err
	}

	err = utils.CopyFile(url, tmpFile.Name(), 0644)

	return tmpFile.Name(), err
}

// WithNodeFilter option sets a filter for nodes to be deployed.
// A filter is a list of node names to be deployed,
// names are provided exactly as they are listed in the topology file.
// Since this is altering the clab.config.Topology.[Nodes,Links] it must only
// be called after WithTopoFile.
func WithNodeFilter(nodeFilter []string) ClabOption {
	return func(c *CLab) error {
		return c.filterClabNodes(nodeFilter)
	}
}

func (c *CLab) filterClabNodes(nodeFilter []string) error {
	if len(nodeFilter) == 0 {
		return nil
	}

	c.nodeFilter = nodeFilter

	// ensure that the node filter is a subset of the nodes in the topology
	for _, n := range nodeFilter {
		if _, ok := c.Config.Topology.Nodes[n]; !ok {
			return fmt.Errorf("%w: node %q is not present in the topology", errs.ErrIncorrectInput, n)
		}
	}

	log.Infof("Applying node filter: %q", nodeFilter)

	// filter nodes
	for name := range c.Config.Topology.Nodes {
		if exists := slices.Contains(nodeFilter, name); !exists {
			log.Debugf("Excluding node %s", name)
			delete(c.Config.Topology.Nodes, name)
		}
	}

	return nil
}

// NewContainerLab function defines a new container lab.
func NewContainerLab(opts ...ClabOption) (*CLab, error) {
	c := &CLab{
		Config: &Config{
			Mgmt:     new(types.MgmtNet),
			Topology: types.NewTopology(),
		},
		m:        new(sync.RWMutex),
		Nodes:    make(map[string]nodes.Node),
		Links:    make(map[int]links.Link),
		Runtimes: make(map[string]runtime.ContainerRuntime),
		Cert:     &cert.Cert{},
	}

	// init a new NodeRegistry
	c.Reg = nodes.NewNodeRegistry()

	// register all nodes
	c.RegisterNodes()

	for _, opt := range opts {
		err := opt(c)
		if err != nil {
			return nil, err
		}
	}

	var err error
	if c.TopoPaths.TopologyFileIsSet() {
		err = c.parseTopology()
	}

	// Extract the host systems DNS servers and populate the
	// Nodes DNS Config with these if not specifically provided
	fileSystem := os.DirFS("/")
	if err := c.ExtractDNSServers(fileSystem); err != nil {
		return nil, err
	}

	return c, err
}

// initMgmtNetwork sets management network config.
func (c *CLab) initMgmtNetwork() error {
	log.Debugf("method initMgmtNetwork was called mgmt params %+v", c.Config.Mgmt)
	if c.Config.Mgmt.Network == "" {
		c.Config.Mgmt.Network = dockerNetName
	}

	if c.Config.Mgmt.IPv4Subnet == "" && c.Config.Mgmt.IPv6Subnet == "" {
		c.Config.Mgmt.IPv4Subnet = dockerNetIPv4Addr
		c.Config.Mgmt.IPv6Subnet = dockerNetIPv6Addr
	}

	// by default external access is enabled if not set by a user
	if c.Config.Mgmt.ExternalAccess == nil {
		c.Config.Mgmt.ExternalAccess = new(bool)
		*c.Config.Mgmt.ExternalAccess = true
	}

	log.Debugf("New mgmt params are %+v", c.Config.Mgmt)

	return nil
}

func (c *CLab) GlobalRuntime() runtime.ContainerRuntime {
	return c.Runtimes[c.globalRuntime]
}

// CreateNodes schedules nodes creation and returns a waitgroup for all nodes.
// Nodes interdependencies are created in this function.
func (c *CLab) CreateNodes(ctx context.Context, maxWorkers uint,
	dm dependency_manager.DependencyManager,
) (*sync.WaitGroup, error) {
	for nodeName := range c.Nodes {
		dm.AddNode(nodeName)
	}

	// nodes with static mgmt IP should be scheduled before the dynamic ones
	err := createStaticDynamicDependency(c.Nodes, dm)
	if err != nil {
		return nil, err
	}

	// create user-defined node dependencies done with `wait-for` node property
	err = createWaitForDependency(c.Nodes, dm)
	if err != nil {
		return nil, err
	}

	// create a set of dependencies, that makes the ignite nodes start one after the other
	err = createIgniteSerialDependency(c.Nodes, dm)
	if err != nil {
		return nil, err
	}

	// make network namespace shared containers start in the right order
	createNamespaceSharingDependency(c.Nodes, dm)

	// Add possible additional dependencies here

	// make sure that there are no unresolvable dependencies, which would deadlock.
	err = dm.CheckAcyclicity()
	if err != nil {
		return nil, err
	}

	// start scheduling
	NodesWg := c.scheduleNodes(ctx, int(maxWorkers), c.Nodes, dm)

	return NodesWg, nil
}

// create a set of dependencies, that makes the ignite nodes start one after the other.
func createIgniteSerialDependency(nodeMap map[string]nodes.Node, dm dependency_manager.DependencyManager) error {
	var prevIgniteNode nodes.Node
	// iterate through the nodes
	for _, n := range nodeMap {
		// find nodes that should run with IgniteRuntime
		if n.GetRuntime().GetName() == ignite.RuntimeName {
			if prevIgniteNode != nil {
				// add a dependency to the previously found ignite node
				dm.AddDependency(n.Config().ShortName, prevIgniteNode.Config().ShortName)
			}
			prevIgniteNode = n
		}
	}
	return nil
}

// createNamespaceSharingDependency adds dependency between the containerlab nodes that share a common network namespace.
// When a node_a in the topology configured to be started in the netns of a node_b as such:
//
// node_a:
//
//	network-mode: container:node_b
//
// then node_a depends on node_b, and waits for node_b to be scheduled first.
func createNamespaceSharingDependency(nodeMap map[string]nodes.Node, dm dependency_manager.DependencyManager) {
	for nodeName, n := range nodeMap {
		nodeConfig := n.Config()
		netModeArr := strings.SplitN(nodeConfig.NetworkMode, ":", 2)
		if netModeArr[0] != "container" {
			// we only care about nodes with shared netns network-mode ("container:<CONTAINERNAME>")
			continue
		}
		// the referenced container might be an external pre-existing or a container defined in the given clab topology.
		referencedNode := netModeArr[1]

		// if the container does not exist in the list of containers, it must be an external dependency
		// it can be ignored for internal processing so -> continue
		if _, exists := nodeMap[netModeArr[1]]; !exists {
			continue
		}

		// since the referenced container is clab-managed node, we create a dependency between the nodes
		dm.AddDependency(referencedNode, nodeName)
	}
}

// createStaticDynamicDependency creates the dependencies between the nodes such that all nodes with dynamic mgmt IP
// are dependent on the nodes with static mgmt IP. This results in nodes with static mgmt IP to be scheduled before dynamic ones.
func createStaticDynamicDependency(n map[string]nodes.Node, dm dependency_manager.DependencyManager) error {
	staticIPNodes := make(map[string]nodes.Node)
	dynIPNodes := make(map[string]nodes.Node)

	// divide the nodes into static and dynamic mgmt IP nodes.
	for name, n := range n {
		if n.Config().MgmtIPv4Address != "" || n.Config().MgmtIPv6Address != "" {
			staticIPNodes[name] = n
			continue
		}
		dynIPNodes[name] = n
	}

	// go through all the dynamic ip nodes
	for dynNodeName := range dynIPNodes {
		// and add their wait group to the the static nodes, while increasing the waitgroup
		for staticNodeName := range staticIPNodes {
			err := dm.AddDependency(staticNodeName, dynNodeName)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// createWaitForDependency reflects the dependencies defined in the configuration via the wait-for field.
func createWaitForDependency(n map[string]nodes.Node, dm dependency_manager.DependencyManager) error {
	for waiterNode, node := range n {
		// add node's waitFor nodes to the dependency manager
		for _, waitForNode := range node.Config().WaitFor {
			err := dm.AddDependency(waitForNode, waiterNode)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func (c *CLab) scheduleNodes(ctx context.Context, maxWorkers int,
	scheduledNodes map[string]nodes.Node, dm dependency_manager.DependencyManager,
) *sync.WaitGroup {
	concurrentChan := make(chan nodes.Node)

	workerFunc := func(i int, input chan nodes.Node, wg *sync.WaitGroup,
		dm dependency_manager.DependencyManager,
	) {
		defer wg.Done()
		for {
			select {
			case node, ok := <-input:
				if node == nil || !ok {
					log.Debugf("Worker %d terminating...", i)
					return
				}
				log.Debugf("Worker %d received node: %+v", i, node.Config())

				// Apply startup delay
				delay := node.Config().StartupDelay
				if delay > 0 {
					log.Infof("node %q is being delayed for %d seconds", node.Config().ShortName, delay)
					time.Sleep(time.Duration(delay) * time.Second)
				}

				// PreDeploy
				err := node.PreDeploy(
					ctx,
					&nodes.PreDeployParams{
						Cert:         c.Cert,
						TopologyName: c.Config.Name,
						TopoPaths:    c.TopoPaths,
						SSHPubKeys:   c.SSHPubKeys,
					},
				)
				if err != nil {
					log.Errorf("failed pre-deploy phase for node %q: %v", node.Config().ShortName, err)
					continue
				}
				// Deploy
				err = node.Deploy(ctx, &nodes.DeployParams{})
				if err != nil {
					log.Errorf("failed deploy phase for node %q: %v", node.Config().ShortName, err)
					continue
				}

				err = node.DeployLinks(ctx)
				if err != nil {
					log.Errorf("failed deploy links for node %q: %v", node.Config().ShortName, err)
					continue
				}

				// signal to dependency manager that this node is done with creation
				dm.SignalDone(node.Config().ShortName, dependency_manager.NodeStateCreated)

			case <-ctx.Done():
				return
			}
		}
	}

	numScheduledNodes := len(scheduledNodes)
	if numScheduledNodes < maxWorkers {
		maxWorkers = numScheduledNodes
	}
	wg := new(sync.WaitGroup)

	// start concurrent workers
	wg.Add(int(maxWorkers))
	// it's safe to not check if all nodes are serial because in that case
	// maxWorkers will be 0
	for i := 0; i < maxWorkers; i++ {
		go workerFunc(i, concurrentChan, wg, dm)
	}

	// Waitgroup used to protect the channel towards the workers of being closed to early
	workerFuncChWG := new(sync.WaitGroup)

	// schedule nodes via a go func to create links in parallel
	go func() {
		for _, n := range scheduledNodes {
			workerFuncChWG.Add(1)
			// start a func for all the containers, then will wait for their own waitgroups
			// to be set to zero by their depending containers, then enqueue to the creation channel
			go func(node nodes.Node, dm dependency_manager.DependencyManager,
				workerChan chan<- nodes.Node, wfcwg *sync.WaitGroup,
			) {
				// wait for all the nodes that node depends on
				err := dm.WaitForNodeDependencies(node.Config().ShortName)
				if err != nil {
					log.Error(err)
				}
				// wait for possible external dependencies
				c.WaitForExternalNodeDependencies(ctx, node.Config().ShortName)
				// when all nodes that this node depends on are created, push it into the channel
				workerChan <- node
				// indicate we are done, such that only when all of these functions are done, the workerChan is being closed
				wfcwg.Done()
			}(n, dm, concurrentChan, workerFuncChWG) // execute this function straight away
		}

		// Gate to make sure the channel is not closed before all the nodes made it though the channel
		workerFuncChWG.Wait()
		// close the channel and thereby terminate the workerFuncs
		close(concurrentChan)
	}()
	return wg
}

// WaitForExternalNodeDependencies makes nodes that have a reference to an external container network-namespace (network-mode: container:<NAME>)
// to wait until the referenced container is in started status.
// The wait time is 15 minutes by default.
func (c *CLab) WaitForExternalNodeDependencies(ctx context.Context, nodeName string) {
	if _, exists := c.Nodes[nodeName]; !exists {
		log.Errorf("unable to find referenced node %q", nodeName)
		return
	}
	nodeConfig := c.Nodes[nodeName].Config()
	netModeArr := strings.SplitN(nodeConfig.NetworkMode, ":", 2)
	if netModeArr[0] != "container" {
		// we only care about nodes with NetMode "container:<CONTAINERNAME>"
		return
	}
	// the referenced container might be an external pre-existing or a container created also by the given clab topology.
	contName := netModeArr[1]

	// if the container does not exist in the list of container, it must be an external dependency
	// it can be ignored for internal processing so -> continue
	if _, exists := c.Nodes[contName]; exists {
		return
	}

	runtime.WaitForContainerRunning(ctx, c.Runtimes[c.globalRuntime], contName, nodeName)
}

func (c *CLab) DeleteNodes(ctx context.Context, workers uint, serialNodes map[string]struct{}) {
	wg := new(sync.WaitGroup)

	concurrentChan := make(chan nodes.Node)
	serialChan := make(chan nodes.Node)

	workerFunc := func(i uint, input chan nodes.Node, wg *sync.WaitGroup) {
		defer wg.Done()
		for {
			select {
			case n := <-input:
				if n == nil {
					log.Debugf("Worker %d terminating...", i)
					return
				}
				err := n.Delete(ctx)
				if err != nil {
					log.Errorf("could not remove container %q: %v", n.Config().LongName, err)
				}
			case <-ctx.Done():
				return
			}
		}
	}

	// start concurrent workers
	wg.Add(int(workers))
	for i := uint(0); i < workers; i++ {
		go workerFunc(i, concurrentChan, wg)
	}

	// start the serial worker
	if len(serialNodes) > 0 {
		wg.Add(1)
		go workerFunc(workers, serialChan, wg)
	}

	// send nodes to workers
	for _, n := range c.Nodes {
		if _, ok := serialNodes[n.Config().LongName]; ok {
			serialChan <- n
			continue
		}
		concurrentChan <- n
	}

	// close channel to terminate the workers
	close(concurrentChan)
	close(serialChan)

	// also call delete on the special nodes
	for _, n := range c.GetSpecialLinkNodes() {
		err := n.Delete(ctx)
		if err != nil {
			log.Warn(err)
		}
	}

	wg.Wait()
}

// ListContainers lists all containers using provided filter.
func (c *CLab) ListContainers(ctx context.Context, filter []*types.GenericFilter) ([]runtime.GenericContainer, error) {
	var containers []runtime.GenericContainer

	for _, r := range c.Runtimes {
		ctrs, err := r.ListContainers(ctx, filter)
		if err != nil {
			return containers, fmt.Errorf("could not list containers: %v", err)
		}
		containers = append(containers, ctrs...)
	}
	return containers, nil
}

// ListNodesContainers lists all containers based on the nodes stored in clab instance.
func (c *CLab) ListNodesContainers(ctx context.Context) ([]runtime.GenericContainer, error) {
	var containers []runtime.GenericContainer

	for _, n := range c.Nodes {
		cts, err := n.GetContainers(ctx)
		if err != nil {
			return containers, fmt.Errorf("could not get container for node %s: %v", n.Config().LongName, err)
		}

		containers = append(containers, cts...)
	}

	return containers, nil
}

// ListNodesContainersIgnoreNotFound lists all containers based on the nodes stored in clab instance, ignoring errors for non found containers.
func (c *CLab) ListNodesContainersIgnoreNotFound(ctx context.Context) ([]runtime.GenericContainer, error) {
	var containers []runtime.GenericContainer

	for _, n := range c.Nodes {
		cts, err := n.GetContainers(ctx)
		if err != nil {
			continue
		}
		containers = append(containers, cts...)
	}

	return containers, nil
}

func (c *CLab) GetNodeRuntime(contName string) (runtime.ContainerRuntime, error) {
	shortName, err := getShortName(c.Config.Name, c.Config.Prefix, contName)
	if err != nil {
		return nil, err
	}

	if node, ok := c.Nodes[shortName]; ok {
		return node.GetRuntime(), nil
	}

	return nil, fmt.Errorf("could not find a container matching name %q", contName)
}

// GetLinkNodes returns all CLab.Nodes nodes as links.Nodes enriched with the special nodes - host and mgmt-net.
// The CLab nodes are copied to a new map and thus clab.Node interface is converted to link.Node.
func (c *CLab) GetLinkNodes() map[string]links.Node {
	// resolveNodes is a map of all nodes in the topology
	// that is artificially created to combat circular dependencies.
	// If no circ deps were in place we could've used c.Nodes map instead.
	// The map is used to resolve links between the nodes by passing it in the ResolveParams struct.
	resolveNodes := make(map[string]links.Node, len(c.Nodes))
	for k, v := range c.Nodes {
		resolveNodes[k] = v
	}

	// add the virtual host and mgmt-bridge nodes to the resolve nodes
	specialNodes := c.GetSpecialLinkNodes()
	for _, n := range specialNodes {
		resolveNodes[n.GetShortName()] = n
	}

	return resolveNodes
}

// GetSpecialLinkNodes returns a map of special nodes that are used to resolve links.
// Special nodes are host and mgmt-bridge nodes that are not typically present in the topology file
// but are required to resolve links.
func (c *CLab) GetSpecialLinkNodes() map[string]links.Node {
	// add the virtual host and mgmt-bridge nodes to the resolve nodes
	specialNodes := map[string]links.Node{
		"host":     links.GetHostLinkNode(),
		"mgmt-net": links.GetMgmtBrLinkNode(),
	}

	return specialNodes
}

// ResolveLinks resolves raw links to the actual link types and stores them in the CLab.Links map.
func (c *CLab) ResolveLinks() error {
	resolveParams := &links.ResolveParams{
		Nodes:          c.GetLinkNodes(),
		MgmtBridgeName: c.Config.Mgmt.Bridge,
		NodesFilter:    c.nodeFilter,
	}

	for i, l := range c.Config.Topology.Links {
		l, err := l.Link.Resolve(resolveParams)
		if err != nil {
			return err
		}

		// if the link is nil, it means that it was filtered out
		if l == nil {
			continue
		}

		c.Endpoints = append(c.Endpoints, l.GetEndpoints()...)
		c.Links[i] = l
	}

	return nil
}

// ExtractDNSServers extracts DNS servers from the resolv.conf files
// and populates the Nodes DNS Config with these if not specifically provided.
func (c *CLab) ExtractDNSServers(filesys fs.FS) error {
	// extract DNS servers from the relevant resolv.conf files
	DNSServers, err := utils.ExtractDNSServersFromResolvConf(filesys,
		[]string{"etc/resolv.conf", "run/systemd/resolve/resolv.conf"})
	if err != nil {
		return err
	}

	// no DNS Servers found, return
	if len(DNSServers) == 0 {
		return nil
	}

	// if no dns servers are explicitly configured,
	// we set the DNS servers that we've extracted.
	for _, n := range c.Nodes {
		config := n.Config()
		// skip nodes in container network mode since docker doesn't allow
		// setting dns config for them
		if strings.HasPrefix(config.NetworkMode, "container") {
			log.Debugf("Skipping DNS config for node %s as it is in container network mode", config.ShortName)
			continue
		}

		if config.DNS == nil {
			config.DNS = &types.DNSConfig{}
		}

		if n.Config().DNS.Servers == nil {
			n.Config().DNS.Servers = DNSServers
		}
	}

	return nil
}
