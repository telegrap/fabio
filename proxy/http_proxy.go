package proxy

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/eBay/fabio/config"
	"github.com/eBay/fabio/logger"
	"github.com/eBay/fabio/metrics"
	"github.com/eBay/fabio/proxy/gzip"
	"github.com/eBay/fabio/route"
)

// HTTPProxy is a dynamic reverse proxy for HTTP and HTTPS protocols.
type HTTPProxy struct {
	// Config is the proxy configuration as provided during startup.
	Config config.Proxy

	// Transport is the http connection pool configured with timeouts.
	// The proxy will panic if this value is nil.
	Transport http.RoundTripper

	// Lookup returns a target host for the given request.
	// The proxy will panic if this value is nil.
	Lookup func(*http.Request) *route.Target

	// Requests is a timer metric which is updated for every request.
	Requests metrics.Timer

	// Noroute is a counter metric which is updated for every request
	// where Lookup() returns nil.
	Noroute metrics.Counter

	// Logger is the access logger for the requests.
	Logger logger.HTTPLogger
}

func (p *HTTPProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p.Lookup == nil {
		panic("no lookup function")
	}

	t := p.Lookup(r)
	if t == nil {
		w.WriteHeader(p.Config.NoRouteStatus)
		return
	}

	if err := addHeaders(r, p.Config); err != nil {
		http.Error(w, "cannot parse "+r.RemoteAddr, http.StatusInternalServerError)
		return
	}

	// TODO(fs): The HasPrefix check seems redundant since the lookup function should
	// TODO(fs): have found the target based on the prefix but there may be other
	// TODO(fs): matchers which may have different rules. I'll keep this for
	// TODO(fs): a defensive approach.
	if t.StripPath != "" && strings.HasPrefix(r.URL.Path, t.StripPath) {
		r.URL.Path = r.URL.Path[len(t.StripPath):]
	}

	upgrade, accept := r.Header.Get("Upgrade"), r.Header.Get("Accept")

	var h http.Handler
	switch {
	case upgrade == "websocket" || upgrade == "Websocket":
		h = newRawProxy(t.URL)

	case accept == "text/event-stream":
		// use the flush interval for SSE (server-sent events)
		// must be > 0s to be effective
		h = newHTTPProxy(t.URL, p.Transport, p.Config.FlushInterval)

	default:
		h = newHTTPProxy(t.URL, p.Transport, time.Duration(0))
	}

	if p.Config.GZIPContentTypes != nil {
		h = gzip.NewGzipHandler(h, p.Config.GZIPContentTypes)
	}

	start := time.Now()
	h.ServeHTTP(w, r)
	if p.Requests != nil {
		p.Requests.UpdateSince(start)
	}
	t.Timer.UpdateSince(start)

	if hr, ok := h.(responser); ok {
		if resp := hr.response(); resp != nil {
			name := key(resp.StatusCode)
			metrics.DefaultRegistry.GetTimer(name).UpdateSince(start)
			if p.Logger != nil {
				p.Logger.Log(&logger.Event{
					Start:        start,
					End:          time.Now(),
					Req:          r,
					Resp:         resp,
					UpstreamAddr: t.URL.Host,
					UpstreamURL:  t.URL,
				})
			}
		}
	}
}

func key(code int) string {
	b := []byte("http.status.")
	b = strconv.AppendInt(b, int64(code), 10)
	return string(b)
}
