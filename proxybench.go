// Package proxybench provides a mechanism for benchmarking proxies that are
// running with the -bench flag.
package proxybench

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/getlantern/golog"
	"github.com/getlantern/netx"
	"github.com/getlantern/ops"
	"github.com/oxtoacart/bpool"

	"git.torproject.org/pluggable-transports/goptlib.git"
	"git.torproject.org/pluggable-transports/obfs4.git/transports/obfs4"
)

var (
	log          = golog.LoggerFor("proxybench")
	buffers      = bpool.NewBytePool(10, 65536)
	protocols    = []string{"https", "obfs4"}
	testingProxy = ""
)

type Proxy struct {
	Addrs      map[string]string `json:"addrs"`
	Provider   string            `json:"provider"`
	DataCenter string            `json:"dataCenter"`
}

func (p *Proxy) withRandomProtocol() *proxy {
	protocol := protocols[rand.Intn(len(protocols))]
	return &proxy{p, protocol, p.Addrs[protocol]}
}

type proxy struct {
	*Proxy
	protocol string
	addr     string
}

type Opts struct {
	SampleRate   float64 `json:"sampleRate"`
	Period       time.Duration
	PeriodString string   `json:"period"`
	Proxies      []*Proxy `json:"proxies"`
	URLs         []string `json:"urls"`
	UpdateURL    string   `json:"updateURL"`
}

func (opts *Opts) applyDefaults() {
	testingMode := testingProxy != ""
	if opts.PeriodString != "" {
		opts.Period, _ = time.ParseDuration(opts.PeriodString)
	}
	if opts.Period <= 0 {
		opts.Period = 1 * time.Hour
	}
	if opts.SampleRate <= 0 {
		opts.SampleRate = 0.05 // 5%
	}
	if opts.UpdateURL == "" && !testingMode {
		opts.UpdateURL = "https://s3.amazonaws.com/lantern/proxybench.json"
	}
	if len(opts.URLs) == 0 {
		opts.URLs = []string{
			"https://www.google.com/humans.txt",
			"https://www.facebook.com/humans.txt",
			"https://67.media.tumblr.com/avatar_4adfafc4c768_48.png",
			"http://s.ytimg.com/yts/cssbin/www-pageframe-webp-vfl37g48Z.css", // YouTube
			"http://149.154.167.91/",                                         // Telegram Instant Messenger
		}
	}
	if testingMode {
		log.Debug("Overriding urls and proxy in testing mode")
		opts.SampleRate = 1
		opts.URLs = []string{"http://i.ytimg.com/vi/video_id/0.jpg"}
		opts.Proxies = []*Proxy{&Proxy{Addrs: map[string]string{"https": testingProxy}, Provider: "testingProvider", DataCenter: "testingDC"}}
	}
}

type ReportFN func(timing time.Duration, ctx map[string]interface{})

func Start(opts *Opts, report ReportFN) {
	opts.applyDefaults()

	ops.Go(func() {
		for {
			opts = opts.fetchUpdate()
			if rand.Float64() < opts.SampleRate {
				log.Debugf("Running benchmarks")
				bench(opts, report)
			}
			// Add +/- 20% to sleep time
			sleepPeriod := time.Duration(float64(opts.Period) * (1.0 + (rand.Float64()-1.0)/5))
			log.Debugf("Waiting %v before running again", sleepPeriod)
			time.Sleep(sleepPeriod)
		}
	})
}

func bench(opts *Opts, report ReportFN) {
	for _, origin := range opts.URLs {
		for _, proxy := range opts.Proxies {
			request(report, origin, proxy.withRandomProtocol())
		}
	}
}

func request(report ReportFN, origin string, proxy *proxy) {
	// http.Transport can't talk to HTTPS proxies, so we need an intermediary.
	l, err := setupLocalProxy(proxy)
	if err != nil {
		log.Errorf("Unable to set up local proxy for %v: %v", proxy.addr, err)
		return
	}
	doRequest(report, origin, proxy, l.Addr().String())
	l.Close()
}

func doRequest(report ReportFN, origin string, proxy *proxy, addr string) {
	op := ops.Begin("proxybench").
		Set("url", origin).
		Set("proxy_type", "chained").
		Set("proxy_protocol", proxy.protocol).
		Set("proxy_provider", proxy.Provider).
		Set("proxy_datacenter", proxy.DataCenter)
	defer op.End()
	host, port, _ := net.SplitHostPort(proxy.addr)
	op.Set("proxy_host", host).Set("proxy_port", port)

	log.Debug("Making request")
	client := &http.Client{
		Timeout: 1 * time.Minute,
		Transport: &http.Transport{
			Proxy: func(req *http.Request) (*url.URL, error) {
				// Note - we're using HTTP here, but this is talking to the local proxy,
				// which talks HTTPS to the remote proxy.
				return url.Parse("http://" + addr)
			},
			DisableKeepAlives: true,
		},
	}
	defer op.End()
	start := time.Now()
	req, err := http.NewRequest("GET", origin, nil)
	if err != nil {
		log.Debugf("Unable to build request for %v: %v", origin, err)
		return
	}
	op.Set("origin", req.URL.Host).Set("origin_host", req.URL.Host)
	resp, err := client.Do(req)
	if err != nil {
		log.Debugf("Error fetching %v from %v: %v", origin, proxy, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode == 403 || resp.StatusCode == 500 {
		log.Debugf("Unexpected status %v fetching %v from %v: %v", resp.Status, origin, proxy, err)
		return
	}
	// Read the full response body
	io.Copy(ioutil.Discard, resp.Body)
	delta := time.Now().Sub(start)
	op.Set("proxybench_success", true)
	log.Debugf("Request succeeded in %v", delta)
	report(delta, ops.AsMap(op, true))
}

func setupLocalProxy(proxy *proxy) (net.Listener, error) {
	l, err := net.Listen("tcp", "localhost:")
	if err != nil {
		return nil, err
	}
	go func() {
		in, err := l.Accept()
		if err != nil {
			log.Errorf("Unable to accept connection: %v", err)
			return
		}
		go doLocalProxy(in, proxy)
	}()
	return l, nil
}

func doLocalProxy(in net.Conn, proxy *proxy) {
	defer in.Close()
	out, err := proxy.dial()
	if err != nil {
		log.Debugf("Unable to dial proxy %v: %v", proxy, err)
		return
	}
	bufOut := buffers.Get()
	bufIn := buffers.Get()
	defer buffers.Put(bufOut)
	defer buffers.Put(bufIn)
	outErr, inErr := netx.BidiCopy(out, in, bufOut, bufIn)
	if outErr != nil {
		log.Debugf("Error copying to local proxy from %v: %v", proxy, outErr)
	}
	if inErr != nil {
		log.Debugf("Error copying from local proxy to %v: %v", proxy, inErr)
	}
}

func (p *proxy) dial() (net.Conn, error) {
	switch p.protocol {
	case "https":
		return p.dialTLS()
	case "obfs4":
		return p.dialOBFS4()
	default:
		return nil, fmt.Errorf("Unknown protocol %v", p.protocol)
	}
}

func (p *proxy) dialTLS() (net.Conn, error) {
	conn, err := netx.Dial("tcp", p.addr)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(conn, &tls.Config{
		InsecureSkipVerify: true,
	})
	return tlsConn, nil
}

func (p *proxy) dialOBFS4() (net.Conn, error) {
	tr := obfs4.Transport{}
	cf, err := tr.ClientFactory("")
	if err != nil {
		return nil, log.Errorf("Unable to create obfs4 client factory: %v", err)
	}

	ptArgs := &pt.Args{}
	ptArgs.Add("cert", "1LYfzzTyz7xsu0bTBUJacwDTLN3NU/gNSjC+pfdRVNuh/LYmtbLOlhZwCfNTKyUVvfMTWQ")
	ptArgs.Add("iat-mode", "0")

	args, err := cf.ParseArgs(ptArgs)
	if err != nil {
		return nil, log.Errorf("Unable to parse client args: %v", err)
	}
	return cf.Dial("tcp", p.addr, netx.Dial, args)
}

func (opts *Opts) fetchUpdate() *Opts {
	if opts.UpdateURL == "" {
		log.Debug("Not fetching updated options")
		return opts
	}
	resp, err := http.Get(opts.UpdateURL)
	if err != nil {
		log.Errorf("Unable to fetch updated Opts from %v: %v", opts.UpdateURL, err)
		return opts
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		log.Errorf("Unexpected response status fetching updated Opts from %v: %v", opts.UpdateURL, resp.Status)
		return opts
	}
	newOpts := &Opts{}
	err = json.NewDecoder(resp.Body).Decode(newOpts)
	if err != nil {
		log.Errorf("Error decoding JSON for updated Opts from %v: %v", opts.UpdateURL, err)
		return opts
	}
	newOpts.applyDefaults()
	log.Debug("Applying updated options")
	return newOpts
}
