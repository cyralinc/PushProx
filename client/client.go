package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ShowMax/go-fqdn"
	"github.com/cyralinc/pushprox/util"
	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/promlog/flag"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	ConfigPushClientDefaultValue = "http://localhost:8050"
	ConfigFilePath               = "config-client.yaml"
	EnvFqdnKey                   = "CYRAL_PUSH_CLIENT_FQDN"
	EnvProxyURL                  = "CYRAL_PUSH_CLIENT_PROXY_URL"
)

var (
	myFqdn           = kingpin.Flag("fqdn", "FQDN to register with").Default(fqdn.Get()).OverrideDefaultFromEnvar(EnvFqdnKey).String()
	scrapeTargetHost = kingpin.Flag("scrape-target-host", "The target host to scrape").Default("").String()
	proxyURL         = kingpin.Flag("proxy-url", "Push proxy to talk to.").Default(ConfigPushClientDefaultValue).OverrideDefaultFromEnvar(EnvProxyURL).String()
	caCertFile       = kingpin.Flag("tls.cacert", "<file> CA certificate to verify peer against").String()
	tlsCert          = kingpin.Flag("tls.cert", "<cert> Client certificate file").String()
	tlsKey           = kingpin.Flag("tls.key", "<key> Private key file").String()
	metricsAddr      = kingpin.Flag("metrics-addr", "Serve Prometheus metrics at this address").Default(":9369").String()
	configFilePath   = kingpin.Flag("config-file", "Config file path (Unused)").Default(ConfigFilePath).String()
)

var (
	scrapeErrorCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pushprox_client_scrape_errors_total",
			Help: "Number of scrape errors",
		},
	)
	pushErrorCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pushprox_client_push_errors_total",
			Help: "Number of push errors",
		},
	)
	pollErrorCounter = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "pushprox_client_poll_errors_total",
			Help: "Number of poll errors",
		},
	)
)

func init() {
	prometheus.MustRegister(pushErrorCounter, pollErrorCounter, scrapeErrorCounter)
}

// Coordinator for scrape requests and responses
type Coordinator struct {
	logger log.Logger
}

func (c *Coordinator) doScrape(request *http.Request, client *http.Client) {
	logger := log.With(c.logger, "scrape_id", request.Header.Get("id"))
	timeout, _ := util.GetHeaderTimeout(request.Header)
	ctx, cancel := context.WithTimeout(request.Context(), timeout)
	defer cancel()
	request = request.WithContext(ctx)
	// We cannot handle https requests at the proxy, as we would only
	// see a CONNECT, so use a URL parameter to trigger it.
	params := request.URL.Query()
	if params.Get("_scheme") == "https" {
		request.URL.Scheme = "https"
		params.Del("_scheme")
		request.URL.RawQuery = params.Encode()
	}

	scrapeResp, err := client.Do(request)
	if err != nil {
		msg := fmt.Sprintf("Failed to scrape %s: %s", request.URL.String(), err)
		level.Warn(logger).Log("msg", "Failed to scrape", "Request URL", request.URL.String(), "err", err)
		scrapeErrorCounter.Inc()
		resp := &http.Response{
			StatusCode: 500,
			Header:     http.Header{},
			Body:       ioutil.NopCloser(strings.NewReader(msg)),
		}
		err = c.doPush(resp, request, client)
		if err != nil {
			pushErrorCounter.Inc()
			level.Warn(logger).Log("msg", "Failed to push failed scrape response:", "err", err)
			return
		}
		level.Info(logger).Log("msg", "Pushed failed scrape response")
		return
	}
	level.Info(logger).Log("msg", "Retrieved scrape response")
	err = c.doPush(scrapeResp, request, client)
	if err != nil {
		pushErrorCounter.Inc()
		level.Warn(logger).Log("msg", "Failed to push scrape response:", "err", err)
		return
	}
	level.Info(logger).Log("msg", "Pushed scrape result")
}

// Report the result of the scrape back up to the proxy.
func (c *Coordinator) doPush(resp *http.Response, origRequest *http.Request, client *http.Client) error {
	resp.Header.Set("id", origRequest.Header.Get("id")) // Link the request and response
	// Remaining scrape deadline.
	deadline, _ := origRequest.Context().Deadline()
	resp.Header.Set("X-Prometheus-Scrape-Timeout", fmt.Sprintf("%f", float64(time.Until(deadline))/1e9))

	base, err := url.Parse(*proxyURL)
	if err != nil {
		return err
	}
	u, err := url.Parse("push")
	if err != nil {
		return err
	}
	url := base.ResolveReference(u)

	buf := &bytes.Buffer{}
	resp.Write(buf)
	request := &http.Request{
		Method:        "POST",
		URL:           url,
		Body:          ioutil.NopCloser(buf),
		ContentLength: int64(buf.Len()),
	}
	request = request.WithContext(origRequest.Context())
	_, err = client.Do(request)
	if err != nil {
		return err
	}
	return nil
}

func loop(c Coordinator, t *http.Transport) error {
	client := &http.Client{Transport: t}
	base, err := url.Parse(*proxyURL)
	if err != nil {
		level.Error(c.logger).Log("msg", "Error parsing url:", "err", err)
		return errors.New("error parsing url")
	}
	u, err := url.Parse("poll")
	if err != nil {
		level.Error(c.logger).Log("msg", "Error parsing url:", "err", err)
		return errors.New("error parsing url poll")
	}
	url := base.ResolveReference(u)
	resp, err := client.Post(url.String(), "", strings.NewReader(*myFqdn))
	if err != nil {
		level.Error(c.logger).Log("msg", "Error polling:", "err", err)
		return errors.New("error polling")
	}
	defer resp.Body.Close()

	request, err := http.ReadRequest(bufio.NewReader(resp.Body))
	if err != nil {
		level.Error(c.logger).Log("msg", "Error reading request:", "err", err)
		return errors.New("error reading request")
	}
	level.Info(c.logger).Log("msg", "Got scrape request", "scrape_id", request.Header.Get("id"), "url", request.URL)

	request.RequestURI = ""

	if *scrapeTargetHost != "" {
		request.URL = util.ReplaceUrlHost(request.URL, *scrapeTargetHost)
		level.Info(c.logger).Log("msg", "Modified scrape target", "scrape_id", request.Header.Get("id"), "url", request.URL)
	}

	go c.doScrape(request, client)

	return nil
}

// decorrelated Jitter increases the maximum jitter based on the last random value.
type decorrelatedJitter struct {
	duration float64 // sleep time
	min      float64 // min sleep time
	cap      float64 // max sleep time
}

func newJitter() decorrelatedJitter {
	rand.Seed(time.Now().UnixNano())
	return decorrelatedJitter{
		duration: 1,
		min:      1,
		cap:      10,
	}
}

func (d *decorrelatedJitter) calc() {
	d.duration = math.Min(d.cap, d.min+rand.Float64()*(d.duration*3-d.min))
}

func (d *decorrelatedJitter) sleep() {
	time.Sleep(time.Duration(d.duration))
}

func main() {
	promlogConfig := promlog.Config{}
	flag.AddFlags(kingpin.CommandLine, &promlogConfig)
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()
	logger := promlog.New(&promlogConfig)
	coordinator := Coordinator{logger: logger}
	if *proxyURL == "" {
		level.Error(coordinator.logger).Log("msg", "--proxy-url flag must be specified.")
		os.Exit(1)
	}
	// Make sure proxyURL ends with a single '/'
	*proxyURL = strings.TrimRight(*proxyURL, "/") + "/"
	level.Info(coordinator.logger).Log("msg", "URL and FQDN info", "proxy_url", *proxyURL, "fqdn", *myFqdn)

	tlsConfig := &tls.Config{}
	if *tlsCert != "" {
		cert, err := tls.LoadX509KeyPair(*tlsCert, *tlsKey)
		if err != nil {
			level.Error(coordinator.logger).Log("msg", "Certificate or Key is invalid", "err", err)
			os.Exit(1)
		}

		// Setup HTTPS client
		tlsConfig.Certificates = []tls.Certificate{cert}

		tlsConfig.BuildNameToCertificate()
	}

	if *caCertFile != "" {
		caCert, err := ioutil.ReadFile(*caCertFile)
		if err != nil {
			level.Error(coordinator.logger).Log("msg", "Not able to read cacert file", "err", err)
			os.Exit(1)
		}
		caCertPool := x509.NewCertPool()
		if ok := caCertPool.AppendCertsFromPEM(caCert); !ok {
			level.Error(coordinator.logger).Log("msg", "Failed to use cacert file as ca certificate")
			os.Exit(1)
		}

		tlsConfig.RootCAs = caCertPool
	}

	if *metricsAddr != "" {
		go func() {
			if err := http.ListenAndServe(*metricsAddr, promhttp.Handler()); err != nil {
				level.Warn(coordinator.logger).Log("msg", "ListenAndServe", "err", err)
			}
		}()
	}

	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
		}).DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       tlsConfig,
	}

	jitter := newJitter()
	for {
		err := loop(coordinator, transport)
		if err != nil {
			pollErrorCounter.Inc()
			jitter.sleep()
			continue
		}
	}
}
