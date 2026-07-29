package card_template_catalog

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type catalogMetrics struct {
	operations    *prometheus.CounterVec
	compile       *prometheus.HistogramVec
	db            *prometheus.CounterVec
	resolve       *prometheus.CounterVec
	cache         *prometheus.CounterVec
	cacheEntries  prometheus.Gauge
	cacheBytes    prometheus.Gauge
	activeTargets prometheus.Gauge
}

func newCatalogMetrics(registerer prometheus.Registerer) *catalogMetrics {
	if registerer == nil {
		registerer = prometheus.DefaultRegisterer
	}
	return &catalogMetrics{
		operations: registerCounterVec(registerer, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_card_catalog_operation_total",
			Help: "Card template catalog control-plane operations by bounded outcome.",
		}, []string{"operation", "result"})),
		compile: registerHistogramVec(registerer, prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "dmwork_card_catalog_compile_seconds",
			Help:    "Card template artifact compile duration by bounded outcome.",
			Buckets: prometheus.DefBuckets,
		}, []string{"result"})),
		db: registerCounterVec(registerer, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_card_catalog_db_total",
			Help: "Card template catalog authoritative database operations by bounded outcome.",
		}, []string{"operation", "result"})),
		resolve: registerCounterVec(registerer, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_card_catalog_resolve_total",
			Help: "Card template runtime resolutions by bounded source and outcome.",
		}, []string{"source", "result"})),
		cache: registerCounterVec(registerer, prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "dmwork_card_catalog_cache_total",
			Help: "Card template compiled-cache operations by bounded outcome.",
		}, []string{"result"})),
		cacheEntries: registerGauge(registerer, prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dmwork_card_catalog_cache_entries",
			Help: "Retained card template compiled-cache entries.",
		})),
		cacheBytes: registerGauge(registerer, prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dmwork_card_catalog_cache_bytes",
			Help: "Retained canonical bytes in the card template compiled cache.",
		})),
		activeTargets: registerGauge(registerer, prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dmwork_card_catalog_active_targets",
			Help: "Authoritative active card template targets observed during startup validation.",
		})),
	}
}

func registerCounterVec(registerer prometheus.Registerer, collector *prometheus.CounterVec) *prometheus.CounterVec {
	if err := registerer.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegistered) {
			if existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.CounterVec); ok {
				return existing
			}
		}
		panic(err)
	}
	return collector
}

func registerHistogramVec(registerer prometheus.Registerer, collector *prometheus.HistogramVec) *prometheus.HistogramVec {
	if err := registerer.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegistered) {
			if existing, ok := alreadyRegistered.ExistingCollector.(*prometheus.HistogramVec); ok {
				return existing
			}
		}
		panic(err)
	}
	return collector
}

func registerGauge(registerer prometheus.Registerer, collector prometheus.Gauge) prometheus.Gauge {
	if err := registerer.Register(collector); err != nil {
		var alreadyRegistered prometheus.AlreadyRegisteredError
		if errors.As(err, &alreadyRegistered) {
			if existing, ok := alreadyRegistered.ExistingCollector.(prometheus.Gauge); ok {
				return existing
			}
		}
		panic(err)
	}
	return collector
}

func (m *catalogMetrics) observeOperation(operation, result string) {
	if m != nil {
		m.operations.WithLabelValues(operation, result).Inc()
	}
}

func (m *catalogMetrics) observeCompile(result string, duration time.Duration) {
	if m != nil {
		m.compile.WithLabelValues(result).Observe(duration.Seconds())
	}
}

func (m *catalogMetrics) observeDB(operation, result string) {
	if m != nil {
		m.db.WithLabelValues(operation, result).Inc()
	}
}

func (m *catalogMetrics) observeResolve(source, result string) {
	if m != nil {
		m.resolve.WithLabelValues(source, result).Inc()
	}
}

func (m *catalogMetrics) observeCache(result string) {
	if m != nil {
		m.cache.WithLabelValues(result).Inc()
	}
}

func (m *catalogMetrics) setCacheSize(entries, bytes int) {
	if m != nil {
		m.cacheEntries.Set(float64(entries))
		m.cacheBytes.Set(float64(bytes))
	}
}

func (m *catalogMetrics) setActiveTargetCount(count int) {
	if m != nil {
		m.activeTargets.Set(float64(count))
	}
}
