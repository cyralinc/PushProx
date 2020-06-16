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
	mu sync.Mutex

	// Clients waiting for a scrape.
	waiting map[string]map[string]chan *http.Request
	// Responses from clients.
	responses map[string]chan *http.Response
	// Clients we know about and when they last contacted us.
	known map[string]time.Time

	logger log.Logger
}

// NewCoordinator initiates the coordinator and starts the client cleanup routine
func NewCoordinator(logger log.Logger) (*Coordinator, error) {
	c := &Coordinator{
		waiting:   map[string]map[string]chan *http.Request{},
		responses: map[string]chan *http.Response{},
		known:     map[string]time.Time{},
		logger:    logger,
	}

	go c.gc()
	return c, nil
}

// Generate a unique ID
func (c *Coordinator) genID() (string, error) {
	id, err := uuid.NewRandom()
	return id.String(), err
}

func (c *Coordinator) getRandomlySelectedRequestChannel(fqdn string) (chan *http.Request, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.waiting[fqdn]; !ok {
		//no requests from prometheus yet
		return nil, fmt.Errorf("no requests from prometheus yet for fqdn %s", fqdn)
	}
	var serviceList []string
	for s := range c.waiting[fqdn] {
		serviceList = append(serviceList, s)
	}
	r := rand.Intn(len(serviceList))
	service := serviceList[r]
	fmt.Printf("Picked service %s for fqdn %s channel %v\n", fqdn, service, c.waiting[fqdn][service])
	return c.waiting[fqdn][service], nil
}

func (c *Coordinator) getPerServiceRequestChannel(sidecar string, service string) chan *http.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.waiting[sidecar]; !ok {
		c.waiting[sidecar] = make(map[string]chan *http.Request)
	}
	if _, ok := c.waiting[sidecar][service]; !ok {
		c.waiting[sidecar][service] = make(chan *http.Request)
	}
	fmt.Printf("service: %s sidecar: %s c.waiting = %+v\n", service, sidecar, c.waiting)
	return c.waiting[sidecar][service]
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
	service := ""
	b := strings.Split(serviceWrapper, ".")
	if len(b) == 2 {
		service = b[0]
		fqdn = b[1]
		r.URL = util.ReplaceUrlHost(r.URL, service)
		r.Host = r.URL.Host
	}

	t := time.Now()
	select {
	case <-ctx.Done():
		d := time.Now().Sub(t)
		fmt.Printf("Timeout reached in duration = %v\n", d)
		return nil, fmt.Errorf("Timeout reached for %q: %s", r.URL.String(), ctx.Err())
	case c.getPerServiceRequestChannel(fqdn, service) <- r:
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

// WaitForScrapeInstruction registers a client waiting for a scrape result
func (c *Coordinator) WaitForScrapeInstruction(fqdn string) (*http.Request, error) {
	level.Info(c.logger).Log("msg", "WaitForScrapeInstruction", "fqdn", fqdn)

	c.addKnownClient(fqdn)
	// TODO: What if the client times out?
	ch, err := c.getRandomlySelectedRequestChannel(fqdn)
	if err != nil {
		level.Info(c.logger).Log("msg", "WaitForScrapeInstruction", "error", err)
		return nil, err
	}

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
					deleted++
				}
			}
			level.Info(c.logger).Log("msg", "GC of clients completed", "deleted", deleted, "remaining", len(c.known))
			knownClients.Set(float64(len(c.known)))
		}()
	}
}
