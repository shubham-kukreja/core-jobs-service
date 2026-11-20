// Package telemetry provides the process's Prometheus metrics and
// OpenTelemetry tracing. Metrics are served on an internal listener only
// (METRICS_ADDR) and never carry user data: labels are kinds, routes,
// methods and status classes, never ids, emails or query strings.
package telemetry

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/shubham-kukreja/core-jobs-service/internal/jobs"
)

const namespace = "core"

// Prometheus owns a registry and the metrics the app records into it.
type Prometheus struct {
	reg *prometheus.Registry

	jobsStarted  *prometheus.CounterVec
	jobsFinished *prometheus.CounterVec
	jobDuration  *prometheus.HistogramVec
	jobsRunning  *prometheus.GaugeVec

	httpRequests *prometheus.CounterVec
	httpDuration *prometheus.HistogramVec
}

// NewPrometheus builds a registry with Go runtime and process collectors.
func NewPrometheus() *Prometheus {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	p := &Prometheus{
		reg: reg,
		jobsStarted: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "jobs", Name: "started_total",
			Help: "Job attempts started, by kind and trigger.",
		}, []string{"kind", "trigger"}),
		jobsFinished: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "jobs", Name: "finished_total",
			Help: "Job attempts finished, by kind, trigger and outcome (queued = retry or snooze).",
		}, []string{"kind", "trigger", "status"}),
		jobDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "jobs", Name: "duration_seconds",
			Help:    "Job attempt duration, by kind and outcome.",
			Buckets: []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 900},
		}, []string{"kind", "status"}),
		jobsRunning: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace, Subsystem: "jobs", Name: "running",
			Help: "Job attempts running in this process, by kind.",
		}, []string{"kind"}),
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Subsystem: "http", Name: "requests_total",
			Help: "HTTP requests, by method, route pattern and status code.",
		}, []string{"method", "route", "code"}),
		httpDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Subsystem: "http", Name: "request_duration_seconds",
			Help:    "HTTP request latency, by method and route pattern.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"method", "route"}),
	}
	reg.MustRegister(p.jobsStarted, p.jobsFinished, p.jobDuration, p.jobsRunning, p.httpRequests, p.httpDuration)
	return p
}

// Registry exposes the registry for extra collectors and tests.
func (p *Prometheus) Registry() *prometheus.Registry { return p.reg }

// Handler serves the Prometheus text exposition.
func (p *Prometheus) Handler() http.Handler {
	return promhttp.HandlerFor(p.reg, promhttp.HandlerOpts{Registry: p.reg})
}

// JobStarted implements jobs.Metrics.
func (p *Prometheus) JobStarted(kind string, trigger jobs.TriggerType) {
	p.jobsStarted.WithLabelValues(kind, string(trigger)).Inc()
	p.jobsRunning.WithLabelValues(kind).Inc()
}

// JobFinished implements jobs.Metrics.
func (p *Prometheus) JobFinished(kind string, trigger jobs.TriggerType, status jobs.Status, d time.Duration) {
	p.jobsRunning.WithLabelValues(kind).Dec()
	p.jobsFinished.WithLabelValues(kind, string(trigger), string(status)).Inc()
	p.jobDuration.WithLabelValues(kind, string(status)).Observe(d.Seconds())
}

// ObserveHTTP implements api.HTTPMetrics. route is the router pattern
// (e.g. /v1/runs/{id}), never the raw path, so cardinality stays bounded.
func (p *Prometheus) ObserveHTTP(method, route string, code int, d time.Duration) {
	p.httpRequests.WithLabelValues(method, route, strconv.Itoa(code)).Inc()
	p.httpDuration.WithLabelValues(method, route).Observe(d.Seconds())
}

// Backlog is one gauge sample from a backlog source.
type Backlog struct {
	Labels    []string // values for the collector's label names
	Count     float64
	OldestAge time.Duration // zero when empty
}

// BacklogFunc reads a backlog at scrape time.
type BacklogFunc func(ctx context.Context) ([]Backlog, error)

// RegisterBacklog adds a gauge pair (<name>_depth, <name>_oldest_seconds)
// computed by fn on every scrape, bounded by a short timeout. A failed read
// is logged and reported as up=0 rather than failing the scrape.
func (p *Prometheus) RegisterBacklog(name, help string, labels []string, fn BacklogFunc, log *slog.Logger) {
	p.reg.MustRegister(&backlogCollector{
		depth:  prometheus.NewDesc(prometheus.BuildFQName(namespace, name, "depth"), help, labels, nil),
		oldest: prometheus.NewDesc(prometheus.BuildFQName(namespace, name, "oldest_seconds"), "Age of the oldest item: "+help, labels, nil),
		up:     prometheus.NewDesc(prometheus.BuildFQName(namespace, name, "up"), "1 if the last backlog read succeeded.", nil, nil),
		fn:     fn,
		log:    log,
	})
}

type backlogCollector struct {
	depth, oldest, up *prometheus.Desc
	fn                BacklogFunc
	log               *slog.Logger
}

func (c *backlogCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.depth
	ch <- c.oldest
	ch <- c.up
}

func (c *backlogCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := c.fn(ctx)
	if err != nil {
		c.log.Warn("backlog metric read failed", "metric", c.depth.String(), "err", err)
		ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.up, prometheus.GaugeValue, 1)
	for _, r := range rows {
		ch <- prometheus.MustNewConstMetric(c.depth, prometheus.GaugeValue, r.Count, r.Labels...)
		ch <- prometheus.MustNewConstMetric(c.oldest, prometheus.GaugeValue, r.OldestAge.Seconds(), r.Labels...)
	}
}

// RegisterPool exports pgxpool statistics.
func (p *Prometheus) RegisterPool(pool *pgxpool.Pool) {
	p.reg.MustRegister(&poolCollector{pool: pool,
		total:    prometheus.NewDesc(namespace+"_db_pool_conns", "Connections in the pool, by state.", []string{"state"}, nil),
		max:      prometheus.NewDesc(namespace+"_db_pool_max_conns", "Configured maximum pool size.", nil, nil),
		waits:    prometheus.NewDesc(namespace+"_db_pool_acquire_waits_total", "Acquires that had to wait for a connection.", nil, nil),
		waitTime: prometheus.NewDesc(namespace+"_db_pool_acquire_wait_seconds_total", "Total time spent waiting to acquire.", nil, nil),
	})
}

type poolCollector struct {
	pool                        *pgxpool.Pool
	total, max, waits, waitTime *prometheus.Desc
}

func (c *poolCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.total
	ch <- c.max
	ch <- c.waits
	ch <- c.waitTime
}

func (c *poolCollector) Collect(ch chan<- prometheus.Metric) {
	s := c.pool.Stat()
	ch <- prometheus.MustNewConstMetric(c.total, prometheus.GaugeValue, float64(s.AcquiredConns()), "acquired")
	ch <- prometheus.MustNewConstMetric(c.total, prometheus.GaugeValue, float64(s.IdleConns()), "idle")
	ch <- prometheus.MustNewConstMetric(c.total, prometheus.GaugeValue, float64(s.ConstructingConns()), "constructing")
	ch <- prometheus.MustNewConstMetric(c.max, prometheus.GaugeValue, float64(s.MaxConns()))
	ch <- prometheus.MustNewConstMetric(c.waits, prometheus.CounterValue, float64(s.EmptyAcquireCount()))
	ch <- prometheus.MustNewConstMetric(c.waitTime, prometheus.CounterValue, s.EmptyAcquireWaitTime().Seconds())
}
