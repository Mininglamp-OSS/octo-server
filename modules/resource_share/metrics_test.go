package resource_share

import (
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/internal/resourceshare"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShareMetricsExposeOnlyBoundedLabels(t *testing.T) {
	registry := prometheus.NewRegistry()
	observer, err := newShareMetrics(registry)
	require.NoError(t, err)
	observer.ObserveTarget("smart-summary", resourceshare.TargetGroup, resourceshare.ShareUnknown)

	families, err := registry.Gather()
	require.NoError(t, err)
	require.Len(t, families, 1)
	family := families[0]
	assert.Equal(t, "octo_resource_share_targets_total", family.GetName())
	require.Len(t, family.Metric, 1)
	labels := family.Metric[0].Label
	labelNames := make([]string, 0, len(labels))
	for _, label := range labels {
		labelNames = append(labelNames, label.GetName())
		joined := strings.ToLower(label.GetName())
		for _, forbidden := range []string{"uid", "space", "resource", "channel", "intent", "proof", "signature"} {
			assert.NotContains(t, joined, forbidden)
		}
	}
	assert.ElementsMatch(t, []string{"provider", "target_kind", "outcome"}, labelNames)
}
