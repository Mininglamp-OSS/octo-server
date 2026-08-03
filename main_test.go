package main

import (
	"sort"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	aireasoningprocess "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/ai_reasoning_process"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
)

func TestGlobalRateLimitExcludePathsIncludesProbeEndpoints(t *testing.T) {
	paths := globalRateLimitExcludePaths()

	require.Contains(t, paths, "/v1/ping")
	require.Contains(t, paths, "/v1/health")
	require.NotContains(t, paths, "/v1/ready")
}

func TestAccessLogIgnorePathsIncludesProbeEndpoints(t *testing.T) {
	paths := ingorePaths()

	require.Contains(t, paths, "/v1/ping")
	require.Contains(t, paths, "/v1/health")
	require.Contains(t, paths, "/v1/ready")
}

func TestInstallCardTmplRegistryRegistersReasoningHistoryAndV3Default(t *testing.T) {
	previousRegistry := cardtmpl.DefaultRegistry()
	previousRegisterer := prometheus.DefaultRegisterer
	previousGatherer := prometheus.DefaultGatherer
	metricsRegistry := prometheus.NewRegistry()
	prometheus.DefaultRegisterer = metricsRegistry
	prometheus.DefaultGatherer = metricsRegistry
	t.Cleanup(func() {
		cardtmpl.SetDefaultRegistry(previousRegistry)
		cardtmpl.SetGlobalMetrics(nil)
		prometheus.DefaultRegisterer = previousRegisterer
		prometheus.DefaultGatherer = previousGatherer
	})

	registry := installCardTmplRegistry()
	var versions []string
	for _, meta := range registry.List() {
		if meta.ID == aireasoningprocess.TemplateID {
			versions = append(versions, meta.Version)
		}
	}
	sort.Strings(versions)
	require.Equal(t, []string{
		aireasoningprocess.TemplateVersionV1,
		aireasoningprocess.TemplateVersionV2,
		aireasoningprocess.TemplateVersionV3,
	}, versions)

	current, err := registry.Lookup(aireasoningprocess.TemplateID, "")
	require.NoError(t, err)
	require.Equal(t, aireasoningprocess.TemplateVersionV3, current.Meta().Version)
	for _, version := range versions {
		_, err := registry.Lookup(aireasoningprocess.TemplateID, version)
		require.NoError(t, err)
	}
}
