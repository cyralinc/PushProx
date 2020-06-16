package main

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	kingpin "gopkg.in/alecthomas/kingpin.v2"

	"github.com/cyralinc/pushprox/util"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	registrationTimeout = kingpin.Flag("registration.timeout", "After how long a registration expires.").Default("5m").Duration()
)

// Coordinator metrics.
var (
	knownClients = promauto.NewGauge(
		prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "clients",
			Help:      "Number of known pushprox clients.",
		},
	)
)

// Coordinator for scrape requests and responses
type Coordinator struct {
	mu          sync.Mutex
	muInstances sync.Mutex

	// Clients (per instance, per sidecar)
	instancesWaiting map[string]map[string]chan *http.Request
	// Clients waiting for a scrape.
	waiting map[string]chan *http.Request
	// Responses from clients.
	responses map[string]chan *http.Response
	// Clients we know about and when they last contacted us.
	known map[string]time.Time

	logger log.Logger
}

// NewCoordinator initiates the coordinator and starts the client cleanup routine
func NewCoordinator(logger log.Logger) (*Coordinator, error) {
	c := &Coordinator{
		instancesWaiting: map[string]map[string]chan *http.Request{},
		waiting:          map[string]chan *http.Request{},
		responses:        map[string]chan *http.Response{},
		known:            map[string]time.Time{},
		logger:           logger,
	}

	go c.gc()
	go c.evenlyDistributeScrapeRequests()
	return c, nil
}

// Generate a unique ID
func (c *Coordinator) genID() (string, error) {
	id, err := uuid.NewRandom()
	return id.String(), err
}

func (c *Coordinator) getPerInstanceRequestChannel(instance, sidecar string) chan *http.Request {
	c.muInstances.Lock()
	defer c.muInstances.Unlock()
	if _, ok := c.instancesWaiting[sidecar]; !ok {
		c.instancesWaiting[sidecar] = make(map[string]chan *http.Request)
	}
	ch, ok := c.instancesWaiting[sidecar][instance]
	if !ok {
		ch = make(chan *http.Request)
		c.instancesWaiting[sidecar][instance] = ch
	}
	return ch
}

func (c *Coordinator) cleanUpPerInstanceRequestChannels(fqdn string) {
	instance, sidecar, err := splitFqdnIntoInstanceAndSidecar(fqdn)
	if err != nil {
		level.Error(c.logger).Log("msg", "cleanUpPerInstanceRequestChannels", err)
		return
	}
	c.muInstances.Lock()
	defer c.muInstances.Unlock()
	if _, ok := c.instancesWaiting[sidecar]; ok {
		c.instancesWaiting[sidecar] = make(map[string]chan *http.Request)
		if _, ok := c.instancesWaiting[sidecar][instance]; ok {
			delete(c.instancesWaiting[sidecar], instance)
		}
		if len(c.instancesWaiting[sidecar]) == 0 {
			delete(c.instancesWaiting, sidecar)
		}
	}
}

func (c *Coordinator) getRequestChannel(fqdn string) chan *http.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.waiting[fqdn]
	if !ok {
		ch = make(chan *http.Request)
		c.waiting[fqdn] = ch
	}
	return ch
}

func (c *Coordinator) getResponseChannel(id string) chan *http.Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.responses[id]
	if !ok {
		ch = make(chan *http.Response)
		c.responses[id] = ch
	}
	return ch
}

// Remove a response channel. Idempotent.
func (c *Coordinator) removeResponseChannel(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.responses, id)
}

func (c *Coordinator) evenlyDistributeScrapeRequests() {
	randomlyPickInstanceChannel := func(fqdn string) (chan *http.Request, error) {
		var instanceList []string
		if instancesMap, ok := c.instancesWaiting[fqdn]; ok {
			for instance := range instancesMap {
				instanceList = append(instanceList, instance)
			}
			if len(instanceList) != 0 {
				i := rand.Intn(len(instanceList))
				instance := instanceList[i]
				return instancesMap[instance], nil
			}
			return nil, fmt.Errorf("failed to find any instance poll request channels for sidecar %s", fqdn)
		}
		//unexpected, losing a request here
		return nil, fmt.Errorf("failed to find any instances for sidecar %s", fqdn)
	}

	for {
		// first check if a poll channels have been created
		// poll channels are created in response to poll requests
		c.muInstances.Lock()
		for fqdn := range c.instancesWaiting {
			ch := c.getRequestChannel(fqdn)
			// now read request from ch in NB way and push to
			// a randomly picked poll channel
			select {
			case req := <-ch:
				instanceCh, err := randomlyPickInstanceChannel(fqdn)
				if err != nil {
					level.Error(c.logger).Log("msg", "Dropping scrape request due to ", err)
				}
				if instanceCh != nil {
					instanceCh <- req
				} else {
					level.Error(c.logger).Log("msg", "Dropping scrape request due to nil instance channel for sidecar ", fqdn)
				}
			default:
				//non-blocking
			}
		}
		c.muInstances.Unlock()
		// prometheus requests come at frequency of about 15s
		// to avoid spinning sleep here
		time.Sleep(1 * time.Second)
	}
}

// DoScrape requests a scrape.
func (c *Coordinator) DoScrape(ctx context.Context, r *http.Request) (*http.Response, error) {
	id, err := c.genID()
	if err != nil {
		return nil, err
	}
	level.Info(c.logger).Log("msg", "DoScrape", "scrape_id", id, "url", r.URL.String())
	r.Header.Add("Id", id)

	//PC: URL.Hostname() will be of form sevicename.wrappername
	//split this and use the wrappername for request channel
	//modify request URL to servicename
	serviceWrapper := r.URL.Hostname()
	fqdn := serviceWrapper
	b := strings.Split(serviceWrapper, ".")
	if len(b) == 2 {
		fqdn = b[1]
		r.URL = util.ReplaceUrlHost(r.URL, b[0])
		r.Host = r.URL.Host
	}

	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("Timeout reached for %q: %s", r.URL.String(), ctx.Err())
	case c.getRequestChannel(fqdn) <- r:
	}

	respCh := c.getResponseChannel(id)
	defer c.removeResponseChannel(id)

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp := <-respCh:
		return resp, nil
	}
}

func splitFqdnIntoInstanceAndSidecar(fqdn string) (string, string, error) {
	index := strings.LastIndexByte(fqdn, '.')

	if index == -1 || index == len(fqdn)-1 {
		return "", "", fmt.Errorf("fqdn not in expected format %s", fqdn)
	}
	return fqdn[0:], fqdn[index+1:], nil
}

// WaitForScrapeInstruction registers a client waiting for a scrape result
func (c *Coordinator) WaitForScrapeInstruction(fqdn string) (*http.Request, error) {
	level.Info(c.logger).Log("msg", "WaitForScrapeInstruction", "fqdn", fqdn)

	c.addKnownClient(fqdn)
	// TODO: What if the client times out?
	instanceID, sidecarName, err := splitFqdnIntoInstanceAndSidecar(fqdn)
	if err != nil {
		level.Error(c.logger).Log("msg", "WaitForScrapeInstruction", err)
		return nil, err
	}

	ch := c.getPerInstanceRequestChannel(instanceID, sidecarName)
	// Following logic to null past request is not required,
	// This existed based on the assumption that there will be just
	// one client polling on a certain fqdn. we have other requirements with asg

	//// exhaust existing poll request (eg. timeouted queues)
	//select
	//case ch <- nil:
	//	//
	//default:
	//	break
	//}

	for {
		request := <-ch
		if request == nil {
			return nil, fmt.Errorf("request is expired")
		}

		select {
		case <-request.Context().Done():
			// Request has timed out, get another one.
		default:
			return request, nil
		}
	}
}

// ScrapeResult send by client
func (c *Coordinator) ScrapeResult(r *http.Response) error {
	id := r.Header.Get("Id")
	level.Info(c.logger).Log("msg", "ScrapeResult", "scrape_id", id)
	ctx, cancel := context.WithTimeout(context.Background(), util.GetScrapeTimeout(maxScrapeTimeout, defaultScrapeTimeout, r.Header))
	defer cancel()
	// Don't expose internal headers.
	r.Header.Del("Id")
	r.Header.Del("X-Prometheus-Scrape-Timeout-Seconds")
	select {
	case c.getResponseChannel(id) <- r:
		return nil
	case <-ctx.Done():
		c.removeResponseChannel(id)
		return ctx.Err()
	}
}

func (c *Coordinator) addKnownClient(fqdn string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.known[fqdn] = time.Now()
	knownClients.Set(float64(len(c.known)))
}

// KnownClients returns a list of alive clients
func (c *Coordinator) KnownClients() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	limit := time.Now().Add(-*registrationTimeout)
	known := make([]string, 0, len(c.known))
	for k, t := range c.known {
		if limit.Before(t) {
			known = append(known, k)
		}
	}
	return known
}

// Garbagee collect old clients.
func (c *Coordinator) gc() {
	for range time.Tick(1 * time.Minute) {
		func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			limit := time.Now().Add(-*registrationTimeout)
			deleted := 0
			for k, ts := range c.known {
				if ts.Before(limit) {
					delete(c.known, k)
					c.cleanUpPerInstanceRequestChannels(k)
					deleted++
				}
			}
			level.Info(c.logger).Log("msg", "GC of clients completed", "deleted", deleted, "remaining", len(c.known))
			knownClients.Set(float64(len(c.known)))
		}()
	}
}
