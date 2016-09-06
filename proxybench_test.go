package proxybench

import (
	"net"
	"net/http"
	"net/http/httputil"
	"sync"
	"testing"
	"time"

	"github.com/getlantern/tlsdefaults"
	"github.com/stretchr/testify/assert"
)

func TestRoundTrip(t *testing.T) {
	// Force https for testing
	origProtocols := protocols
	protocols = []string{"https"}
	defer func() {
		protocols = origProtocols
	}()

	rp := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
		},
	}
	l, err := tlsdefaults.Listen("localhost:", "testkey.pem", "testcert.pem")
	if !assert.NoError(t, err) {
		return
	}
	defer l.Close()
	go http.Serve(l, rp)

	testingProxy = l.Addr().String()

	var timing time.Duration
	var ctx map[string]interface{}
	var mx sync.RWMutex
	var wg sync.WaitGroup
	wg.Add(1)

	Start(&Opts{}, func(_timing time.Duration, _ctx map[string]interface{}) {
		mx.Lock()
		timing = _timing
		ctx = _ctx
		mx.Unlock()
		wg.Done()
	})
	wg.Wait()

	mx.RLock()
	defer mx.RUnlock()
	host, port, _ := net.SplitHostPort(testingProxy)
	assert.True(t, timing > 0)
	assert.Equal(t, true, ctx["proxybench_success"])
	assert.Equal(t, "http://i.ytimg.com/vi/video_id/0.jpg", ctx["url"])
	assert.Equal(t, "chained", ctx["proxy_type"])
	assert.Equal(t, "https", ctx["proxy_protocol"])
	assert.Equal(t, "testingProvider", ctx["proxy_provider"])
	assert.Equal(t, "testingDC", ctx["proxy_datacenter"])
	assert.Equal(t, host, ctx["proxy_host"])
	assert.Equal(t, port, ctx["proxy_port"])
	assert.Equal(t, "i.ytimg.com", ctx["origin"])
	assert.Equal(t, "i.ytimg.com", ctx["origin_host"])
}
