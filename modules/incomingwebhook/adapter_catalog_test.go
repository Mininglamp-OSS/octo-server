package incomingwebhook

import (
	"testing"

	octoi18n "github.com/Mininglamp-OSS/octo-server/pkg/i18n"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdapterExamplesRenderSupportedLanguages(t *testing.T) {
	for _, lang := range octoi18n.SupportedLanguages() {
		t.Run(lang, func(t *testing.T) {
			examples, err := publicAdapterExamples("iwh_abc", "deadbeef", lang)
			require.NoError(t, err)

			keys := make([]string, 0, len(examples))
			urls := publicURLs("iwh_abc", "deadbeef")
			for _, ex := range examples {
				keys = append(keys, ex.Key)
				assert.NotEmpty(t, ex.Title, "title for %s/%s", lang, ex.Key)
				assert.NotEmpty(t, ex.Description, "description for %s/%s", lang, ex.Key)
				assert.NotEmpty(t, ex.Steps, "steps for %s/%s", lang, ex.Key)
				assert.Equal(t, "application/json", ex.ContentType)
				assert.Equal(t, urls[ex.Key], ex.URL, "example URL must stay in sync with urls[%s]", ex.Key)
			}
			assert.Equal(t, []string{"github", "gitlab", "feishu", "multica", "wecom"}, keys)
		})
	}
}

func TestAdapterExamplesAreLocalized(t *testing.T) {
	zh, err := publicAdapterExamples("iwh_abc", "deadbeef", "zh-CN")
	require.NoError(t, err)
	en, err := publicAdapterExamples("iwh_abc", "deadbeef", "en-US")
	require.NoError(t, err)

	require.NotEmpty(t, zh)
	require.NotEmpty(t, en)
	assert.Contains(t, zh[0].Description, "仓库")
	assert.Contains(t, en[0].Description, "repository")
	assert.NotEqual(t, zh[0].Steps[0], en[0].Steps[0])
}
