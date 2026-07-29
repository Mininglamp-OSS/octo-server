package card_template_catalog

import (
	"errors"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

type catalogMetrics struct {
	operations *prometheus.CounterVec
	compile    *prometheus.HistogramVec
	db         *prometheus.CounterVec
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
