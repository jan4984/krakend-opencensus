package gin

import (
	"go.opencensus.io/plugin/ochttp/propagation/b3"
	"net/http"
	"time"

	"github.com/devopsfaith/krakend/config"
	"github.com/devopsfaith/krakend/proxy"
	krakendgin "github.com/devopsfaith/krakend/router/gin"
	"github.com/gin-gonic/gin"
	"go.opencensus.io/plugin/ochttp"
	"go.opencensus.io/plugin/ochttp/propagation/tracecontext"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
	"go.opencensus.io/trace"
	"go.opencensus.io/trace/propagation"

	opencensus "github.com/devopsfaith/krakend-opencensus"
)

// New wraps a handler factory adding some simple instrumentation to the generated handlers
func New(hf krakendgin.HandlerFactory) krakendgin.HandlerFactory {
	return func(cfg *config.EndpointConfig, p proxy.Proxy) gin.HandlerFunc {
		return HandlerFunc(cfg, hf(cfg, p), &tracecontext.HTTPFormat{})
	}
}

func HandlerFunc(cfg *config.EndpointConfig, next gin.HandlerFunc, prop propagation.HTTPFormat) gin.HandlerFunc {
	if !opencensus.IsRouterEnabled() {
		return next
	}
	if prop == nil {
		prop = &b3.HTTPFormat{}
	}
	h := &handler{
		name:        cfg.Endpoint,
		propagation: prop,
		Handler:     next,
		StartOptions: trace.StartOptions{
			SpanKind: trace.SpanKindServer,
		},
	}
	return h.HandlerFunc
}

type handler struct {
	name             string
	propagation      propagation.HTTPFormat
	Handler          gin.HandlerFunc
	StartOptions     trace.StartOptions
	IsPublicEndpoint bool
}

func (h *handler) HandlerFunc(c *gin.Context) {
	var traceEnd, statsEnd func(*http.Response)
	c.Request, traceEnd = h.startTrace(c.Writer, c.Request)
	c.Writer, statsEnd = h.startStats(c.Writer, c.Request)

	c.Set(opencensus.ContextKey, trace.FromContext(c.Request.Context()))
	h.Handler(c)

	resp := &http.Response{StatusCode: c.Writer.Status()}
	statsEnd(resp)
	traceEnd(resp)
}

func (h *handler) startTrace(_ gin.ResponseWriter, r *http.Request) (*http.Request, func(*http.Response)) {
	ctx := r.Context()
	var span *trace.Span
	sc, ok := h.extractSpanContext(r)
	if ok && !h.IsPublicEndpoint {
		ctx, span = trace.StartSpanWithRemoteParent(ctx, h.name, sc, trace.WithSampler(h.StartOptions.Sampler), trace.WithSpanKind(h.StartOptions.SpanKind))
	} else {
		ctx, span = trace.StartSpan(ctx, h.name, trace.WithSampler(h.StartOptions.Sampler), trace.WithSpanKind(h.StartOptions.SpanKind))
		if ok {
			span.AddLink(trace.Link{
				TraceID:    sc.TraceID,
				SpanID:     sc.SpanID,
				Type:       trace.LinkTypeChild,
				Attributes: nil,
			})
		}
	}
	span.AddAttributes(requestAttrs(r)...)
	return r.WithContext(ctx), func(response *http.Response) {
		span.AddAttributes(responseAttrs(response)...)
		span.End()
	}
}

func (h *handler) extractSpanContext(r *http.Request) (trace.SpanContext, bool) {
	return h.propagation.SpanContextFromRequest(r)
}

func (h *handler) startStats(w gin.ResponseWriter, r *http.Request) (gin.ResponseWriter, func(*http.Response)) {
	ctx, _ := tag.New(r.Context(),
		tag.Upsert(ochttp.Host, r.Host),
		tag.Upsert(ochttp.Path, r.URL.Path),
		tag.Upsert(ochttp.Method, r.Method))
	track := &trackingResponseWriter{
		start:          time.Now(),
		ctx:            ctx,
		ResponseWriter: w,
	}
	if r.Body == nil {
		// TODO: Handle cases where ContentLength is not set.
		track.reqSize = -1
	} else if r.ContentLength > 0 {
		track.reqSize = r.ContentLength
	}
	stats.Record(ctx, ochttp.ServerRequestCount.M(1))
	return track, func(_ *http.Response) {
		track.end()
	}
}

func requestAttrs(r *http.Request) []trace.Attribute {
	return []trace.Attribute{
		trace.StringAttribute(ochttp.PathAttribute, r.URL.Path),
		trace.StringAttribute(ochttp.HostAttribute, r.URL.Host),
		trace.StringAttribute(ochttp.MethodAttribute, r.Method),
		trace.StringAttribute(ochttp.UserAgentAttribute, r.UserAgent()),
	}
}

func responseAttrs(resp *http.Response) []trace.Attribute {
	return []trace.Attribute{
		trace.Int64Attribute(ochttp.StatusCodeAttribute, int64(resp.StatusCode)),
	}
}
