package resource_share

import (
	"sync"

	"github.com/Mininglamp-OSS/octo-server/internal/resourceshare"
	"github.com/prometheus/client_golang/prometheus"
)

type shareMetrics struct {
	targets *prometheus.CounterVec
}

func newShareMetrics(registerer prometheus.Registerer) (*shareMetrics, error) {
	targets := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "octo",
		Subsystem: "resource_share",
		Name:      "targets_total",
		Help:      "Resource share target outcomes by reviewed provider and bounded target kind.",
	}, []string{"provider", "target_kind", "outcome"})
	if err := registerer.Register(targets); err != nil {
		return nil, err
	}
	return &shareMetrics{targets: targets}, nil
}

func (m *shareMetrics) ObserveTarget(provider resourceshare.ProviderID, kind resourceshare.TargetKind, outcome resourceshare.ShareOutcome) {
	if m == nil || m.targets == nil {
		return
	}
	m.targets.WithLabelValues(string(provider), string(kind), string(outcome)).Inc()
}

var (
	defaultMetricsOnce sync.Once
	defaultMetrics     *shareMetrics
)

func defaultShareMetrics() *shareMetrics {
	defaultMetricsOnce.Do(func() {
		metrics, err := newShareMetrics(prometheus.DefaultRegisterer)
		if err != nil {
			panic(err)
		}
		defaultMetrics = metrics
	})
	return defaultMetrics
}
