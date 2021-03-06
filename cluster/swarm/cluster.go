package swarm

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/docker/docker/pkg/units"
	"github.com/docker/swarm/cluster"
	"github.com/docker/swarm/discovery"
	"github.com/docker/swarm/scheduler"
	"github.com/docker/swarm/scheduler/node"
	"github.com/docker/swarm/state"
	"github.com/samalba/dockerclient"
)

// Cluster is exported
type Cluster struct {
	sync.RWMutex

	eventHandler   cluster.EventHandler
	engines        map[string]*cluster.Engine
	scheduler      *scheduler.Scheduler
	options        *cluster.Options
	store          *state.Store
	metaContainers map[string]*metaContainer
}

type metaContainer struct {
	Name       string
	Config     dockerclient.ContainerConfig
	HostConfig dockerclient.HostConfig
	Current    *cluster.Engine
	Prev       *cluster.Engine
}

// NewCluster is exported
func NewCluster(scheduler *scheduler.Scheduler, store *state.Store, options *cluster.Options) cluster.Cluster {
	log.WithFields(log.Fields{"name": "swarm"}).Debug("Initializing cluster")

	cluster := &Cluster{
		engines:        make(map[string]*cluster.Engine),
		scheduler:      scheduler,
		options:        options,
		store:          store,
		metaContainers: make(map[string]*metaContainer),
	}

	// get the list of entries from the discovery service
	go func() {
		d, err := discovery.New(options.Discovery, options.Heartbeat)
		if err != nil {
			log.Fatal(err)
		}

		entries, err := d.Fetch()
		if err != nil {
			log.Fatal(err)

		}
		cluster.newEntries(entries)
		go cluster.monitor()
		go d.Watch(cluster.newEntries)
	}()

	return cluster
}

// Handle callbacks for the events
func (c *Cluster) Handle(e *cluster.Event) error {
	if c.eventHandler == nil {
		return nil
	}
	if err := c.eventHandler.Handle(e); err != nil {
		log.Error(err)
	}
	return nil
}

// RegisterEventHandler registers an event handler.
func (c *Cluster) RegisterEventHandler(h cluster.EventHandler) error {
	if c.eventHandler != nil {
		return errors.New("event handler already set")
	}
	c.eventHandler = h
	return nil
}

// CreateContainer aka schedule a brand new container into the cluster.
func (c *Cluster) CreateContainer(config *dockerclient.ContainerConfig, name string) (*cluster.Container, error) {
	c.scheduler.Lock()

	n, err := c.scheduler.SelectNodeForContainer(c.listNodes(), config)
	if err != nil {
		c.scheduler.Unlock()
		return nil, err
	}

	if nn, ok := c.engines[n.ID]; ok {
		nn.AddtoQueue(config, name)
		if meta, ok := c.metaContainers[name]; !ok {
			c.metaContainers[name] = &metaContainer{
				Name:    name,
				Config:  *config,
				Current: nn,
				Prev:    nn,
			}
		} else {
			meta.Prev = meta.Current
			meta.Current = nn
			c.metaContainers[name] = meta
		}
		c.scheduler.Unlock()
		container, err := nn.Create(config, name, true)
		if err != nil {
			return nil, err
		}
		st := &state.RequestedState{
			ID:     container.Id,
			Name:   name,
			Config: config,
		}
		return container, c.store.Add(container.Id, st)
	}

	return nil, nil
}

// RemoveContainer aka Remove a container from the cluster. Containers should
// always be destroyed through the scheduler to guarantee atomicity.
func (c *Cluster) RemoveContainer(container *cluster.Container, force bool) error {
	c.scheduler.Lock()
	defer c.scheduler.Unlock()

	if err := container.Engine.Destroy(container, force); err != nil {
		return err
	}
	delete(c.metaContainers, strings.TrimPrefix(container.Names[0], "/"))
	if err := c.store.Remove(container.Id); err != nil {
		if err == state.ErrNotFound {
			log.Debugf("Container %s not found in the store", container.Id)
			return nil
		}
		return err
	}
	return nil
}

// Entries are Docker Engines
func (c *Cluster) newEntries(entries []*discovery.Entry) {
	for _, entry := range entries {
		go func(m *discovery.Entry) {
			if !c.hasEngine(m.String()) {
				engine := cluster.NewEngine(m.String(), c.options.OvercommitRatio)
				if err := engine.Connect(c.options.TLSConfig); err != nil {
					log.Error(err)
					return
				}
				c.Lock()

				if old, exists := c.engines[engine.ID]; exists {
					c.Unlock()
					if old.IP != engine.IP {
						log.Errorf("ID duplicated. %s shared by %s and %s", engine.ID, old.IP, engine.IP)
					} else {
						log.Errorf("node %q with IP %q is  already registered", engine.Name, engine.IP)
					}
					return
				}
				c.engines[engine.ID] = engine
				if err := engine.RegisterEventHandler(c); err != nil {
					log.Error(err)
					c.Unlock()
					return
				}
				c.Unlock()

			}
		}(entry)
	}
}

func (c *Cluster) hasEngine(addr string) bool {
	c.RLock()
	defer c.RUnlock()

	for _, engine := range c.engines {
		if engine.Addr == addr {
			return true
		}
	}
	return false
}

// Images returns all the images in the cluster.
func (c *Cluster) Images() []*cluster.Image {
	c.RLock()
	defer c.RUnlock()

	out := []*cluster.Image{}
	for _, n := range c.engines {
		out = append(out, n.Images()...)
	}

	return out
}

// Image returns an image with IDOrName in the cluster
func (c *Cluster) Image(IDOrName string) *cluster.Image {
	// Abort immediately if the name is empty.
	if len(IDOrName) == 0 {
		return nil
	}

	c.RLock()
	defer c.RUnlock()
	for _, n := range c.engines {
		if image := n.Image(IDOrName); image != nil {
			return image
		}
	}

	return nil
}

// RemoveImage removes an image from the cluster
func (c *Cluster) RemoveImage(image *cluster.Image) ([]*dockerclient.ImageDelete, error) {
	c.Lock()
	defer c.Unlock()
	return image.Engine.RemoveImage(image)
}

//Start a container
func (c *Cluster) Start(name string, config *dockerclient.HostConfig) error {
	c.Lock()
	defer c.Unlock()
	//container := c.Container(name)
	meta, ok := c.metaContainers[name]
	if !ok {
		return nil
	}
	c.metaContainers[name] = meta
	return meta.Current.Start(meta.Name)
}

// Pull is exported
func (c *Cluster) Pull(name string, callback func(what, status string)) {
	var wg sync.WaitGroup

	c.RLock()
	for _, n := range c.engines {
		wg.Add(1)

		go func(nn *cluster.Engine) {
			defer wg.Done()

			if callback != nil {
				callback(nn.Name, "")
			}
			err := nn.Pull(name)
			if callback != nil {
				if err != nil {
					callback(nn.Name, err.Error())
				} else {
					callback(nn.Name, "downloaded")
				}
			}
		}(n)
	}
	c.RUnlock()

	wg.Wait()
}

// Containers returns all the containers in the cluster.
func (c *Cluster) Containers() []*cluster.Container {
	c.RLock()
	defer c.RUnlock()

	out := []*cluster.Container{}
	for _, n := range c.engines {
		out = append(out, n.Containers()...)
	}

	return out
}

// Container returns the container with IDOrName in the cluster
func (c *Cluster) Container(IDOrName string) *cluster.Container {
	// Abort immediately if the name is empty.
	if len(IDOrName) == 0 {
		return nil
	}

	c.RLock()
	defer c.RUnlock()
	for _, n := range c.engines {
		if container := n.Container(IDOrName); container != nil {
			return container
		}
	}

	return nil
}

// listNodes returns all the engines in the cluster.
func (c *Cluster) listNodes() []*node.Node {
	c.RLock()
	defer c.RUnlock()

	out := make([]*node.Node, 0, len(c.engines))
	for _, n := range c.engines {
		if n.IsHealthy() {
			out = append(out, node.NewNode(n))
		}
	}

	return out
}

// listEngines returns all the engines in the cluster.
func (c *Cluster) listEngines() []*cluster.Engine {
	c.RLock()
	defer c.RUnlock()

	out := make([]*cluster.Engine, 0, len(c.engines))
	for _, n := range c.engines {
		out = append(out, n)
	}
	return out
}

func (c *Cluster) monitor() {
	healthy := make(map[string]*cluster.Engine)
	time.Sleep(5 * time.Second)

	unhealthy := make(map[string]*cluster.Engine)
	for {
		for _, n := range c.engines {
			if n.IsHealthy() {
				healthy[n.ID] = n
			} else {
				unhealthy[n.ID] = n
			}
		}
		time.Sleep(5 * time.Second)
		for _, n := range healthy {
			if !n.IsHealthy() {
				delete(healthy, n.ID)
				unhealthy[n.ID] = n
				log.WithFields(log.Fields{"Node": n.ID}).Info("failing over")
				go c.failover(n)
			}
		}
		for _, n := range unhealthy {
			log.WithFields(log.Fields{"unhealthy": len(unhealthy)}).Info("unhealthy nodes")
			if n.IsHealthy() {
				delete(unhealthy, n.ID)
				healthy[n.ID] = n
				log.WithFields(log.Fields{"Node": n.ID}).Info("adjusting after failover over")
				go c.adjust(n)
			}
		}
	}
}

func (c *Cluster) adjust(e *cluster.Engine) {
	for _, container := range e.Containers() {
		if meta, ok := c.metaContainers[strings.TrimPrefix(container.Names[0], "/")]; ok {
			if meta.Prev != meta.Current {
				meta.Prev.Destroy(meta.Prev.Container(meta.Name), true)
			}
			log.WithFields(log.Fields{"Node": e.ID, "container": meta.Name}).Info("deleteing in reschedule container")
		}
	}
}

func (c *Cluster) failover(e *cluster.Engine) {
	i := 1
	for ; i <= 2; i++ {
		time.Sleep(time.Duration(5*i) * time.Second)
		if e.IsHealthy() {
			break
		}
	}
	if !e.IsHealthy() {
		for _, container := range e.Containers() {
			if container.Info.State.Running {
				if meta, ok := c.metaContainers[strings.TrimPrefix(container.Names[0], "/")]; ok {
					c.CreateContainer(&meta.Config, meta.Name)
					c.Start(meta.Name, &meta.HostConfig)
					log.WithFields(log.Fields{"Node": e.ID, "container": meta.Name}).Info("rescheduling container")
				}
			}
		}
	}
}

// Info is exported
func (c *Cluster) Info() [][2]string {
	info := [][2]string{
		{"\bStrategy", c.scheduler.Strategy()},
		{"\bFilters", c.scheduler.Filters()},
		{"\bNodes", fmt.Sprintf("%d", len(c.engines))},
	}

	engines := c.listEngines()
	sort.Sort(cluster.EngineSorter(engines))

	for _, engine := range engines {
		info = append(info, [2]string{engine.Name, engine.Addr})
		info = append(info, [2]string{" └ Containers", fmt.Sprintf("%d", len(engine.Containers()))})
		info = append(info, [2]string{" └ Reserved CPUs", fmt.Sprintf("%.3f / %d", engine.UsedCpus(), engine.TotalCpus())})
		info = append(info, [2]string{" └ Reserved Memory", fmt.Sprintf("%s / %s", units.BytesSize(float64(engine.UsedMemory())), units.BytesSize(float64(engine.TotalMemory())))})
		labels := make([]string, 0, len(engine.Labels))
		for k, v := range engine.Labels {
			labels = append(labels, k+"="+v)
		}
		sort.Strings(labels)
		info = append(info, [2]string{" └ Labels", fmt.Sprintf("%s", strings.Join(labels, ", "))})
	}

	return info
}

// RANDOMENGINE returns a random engine.
func (c *Cluster) RANDOMENGINE() (*cluster.Engine, error) {
	n, err := c.scheduler.SelectNodeForContainer(c.listNodes(), &dockerclient.ContainerConfig{})
	if err != nil {
		return nil, err
	}
	if n != nil {
		return c.engines[n.ID], nil
	}
	return nil, nil
}
