package main

import (
	"testing"

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
