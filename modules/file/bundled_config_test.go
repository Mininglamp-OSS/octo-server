package file

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// TestBundledDockerConfig_DownloadURL is the equivalent-verification step for
// PR#50 R5 P0: the bundled docker `octo-server.yaml` MUST ship a
// `minio.downloadURL` value that passes `validatePublicDownloadURL` when
// loaded through Viper, with no environment-variable expansion happening in
// the loader (it does not — only `docker compose` expands `${VAR:-default}`
// placeholders, and that happens on a different layer).
//
// The previous head shipped:
//
//	downloadURL: "http://${OCTO_DOMAIN:-octo.local}:${OCTO_MINIO_API_PORT:-29000}"
//
// Viper passed that literal string through, `url.Parse` rejected it as
// `invalid port` and every PresignedPutURL / PresignedGetURL call against
// the bundled stack failed. This test pins a happy-path equivalent that
// would have caught the regression at unit-test time, so we no longer
// depend on a docker-stack smoke run to surface it.
//
// SoT note: as of the docker/octo + docker/tsdd retirement PR, the live
// runtime config no longer lives in this repo (it lives in
// `Mininglamp-OSS/octo-deployment` under `docker/configs/octo-server.yaml`,
// and a parallel guard test there validates the live shipping copy). The
// fixture under `modules/file/testdata/octo-server.yaml` is the last
// snapshot of that file as of the retirement commit, kept here so the
// `validatePublicDownloadURL` contract stays exercised against a realistic
// post-PR#50 yaml shape from this repo's test suite without re-introducing
// a cross-repo dependency. Refresh the fixture when octo-deployment's
// canonical copy changes shape in a way relevant to this contract.
//
// The companion `TS_MINIO_DOWNLOADURL` env override that the docker
// stack ships lets non-default deployments (`OCTO_DOMAIN` /
// `OCTO_MINIO_API_PORT` overridden in `.env`) project their resolved URL
// through Viper's `TS_` env prefix; that path is exercised separately
// in `TestBundledDockerComposeProvidesDownloadURLOverride`.
func TestBundledDockerConfig_DownloadURL(t *testing.T) {
	cfgPath, err := filepath.Abs("testdata/octo-server.yaml")
	if err != nil {
		t.Fatalf("resolve config path: %v", err)
	}

	vp := viper.New()
	vp.SetConfigFile(cfgPath)
	if err := vp.ReadInConfig(); err != nil {
		t.Fatalf("read bundled docker config %s: %v", cfgPath, err)
	}

	got := vp.GetString("minio.downloadURL")
	if got == "" {
		t.Fatalf("bundled docker minio.downloadURL is empty; expected a literal browser-reachable URL")
	}

	// Viper does NOT expand shell `${VAR:-default}` placeholders. If any
	// future edit reintroduces one, validatePublicDownloadURL will reject
	// the literal string at sign time; pin that early here.
	if strings.Contains(got, "${") || strings.Contains(got, "}") {
		t.Fatalf("bundled docker minio.downloadURL contains an unexpanded shell placeholder: %q\n"+
			"Viper does not expand ${VAR:-default}; use a literal URL in the yaml and "+
			"override via TS_MINIO_DOWNLOADURL in docker-compose.yaml when needed.", got)
	}

	if err := validatePublicDownloadURL(got); err != nil {
		t.Fatalf("bundled docker minio.downloadURL %q failed validatePublicDownloadURL: %v", got, err)
	}
}

// TestBundledDockerComposeProvidesDownloadURLOverride pins the second half of
// the P0 fix: when `docker compose` expands the `TS_MINIO_DOWNLOADURL`
// override (with operator-overridden OCTO_DOMAIN / OCTO_MINIO_API_PORT in
// `.env`), the resolved value MUST also pass `validatePublicDownloadURL`.
//
// We exercise the post-compose-expansion shape directly — compose is
// straightforward enough that we don't need to fork a real `docker
// compose config` here; the regression risk this guards against is the
// override accidentally drifting back to a path-prefixed URL or a
// placeholder string. Both shapes are caught by validatePublicDownloadURL.
func TestBundledDockerComposeProvidesDownloadURLOverride(t *testing.T) {
	cases := []struct {
		name string
		// Result of `docker compose` expanding the
		// `TS_MINIO_DOWNLOADURL: "http://${OCTO_DOMAIN:-octo.local}:${OCTO_MINIO_API_PORT:-29000}"`
		// line under different operator `.env` settings.
		expanded string
	}{
		{
			name:     "default env (no overrides)",
			expanded: "http://octo.local:29000",
		},
		{
			name:     "operator overrode OCTO_DOMAIN only",
			expanded: "http://octo.example.com:29000",
		},
		{
			name:     "operator overrode both domain and minio API port",
			expanded: "http://octo.example.com:9000",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validatePublicDownloadURL(tc.expanded); err != nil {
				t.Fatalf("post-compose-expansion %q failed: %v", tc.expanded, err)
			}
		})
	}
}
