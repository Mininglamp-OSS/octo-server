package voice

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLoadPrompts_FileNotFound(t *testing.T) {
	t.Cleanup(resetToDefaults)
	LoadPrompts("/nonexistent/path.yaml", nil)
	assert.Equal(t, systemPrompt, activePrompts.System)
	assert.Equal(t, vocabularyReferenceTemplate, activePrompts.VocabularyReference)
}

func TestLoadPrompts_EmptyPath(t *testing.T) {
	t.Cleanup(resetToDefaults)
	LoadPrompts("", nil)
	assert.Equal(t, systemPrompt, activePrompts.System)
}

func TestLoadPrompts_PartialOverride(t *testing.T) {
	t.Cleanup(resetToDefaults)
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts.yaml")
	os.WriteFile(path, []byte(`system: "custom system prompt"`), 0644)

	LoadPrompts(path, nil)
	assert.Equal(t, "custom system prompt", activePrompts.System)
	// Other fields should remain as defaults
	assert.Equal(t, vocabularyReferenceTemplate, activePrompts.VocabularyReference)
	assert.Equal(t, editInputBufferTemplate, activePrompts.EditInputBuffer)
}

func TestLoadPrompts_FullOverride(t *testing.T) {
	t.Cleanup(resetToDefaults)
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts.yaml")
	content := `
system: "custom system"
vocabulary_reference: "custom vocab %s"
append_input_buffer: "custom append %s"
append_input_buffer_no_vocab: "custom append nv %s"
edit_input_buffer: "custom edit %s"
task_transcribe: "custom task transcribe"
task_transcribe_with_vocab: "custom task transcribe vocab"
task_append: "custom task append"
task_edit: "custom task edit"
`
	os.WriteFile(path, []byte(content), 0644)

	LoadPrompts(path, nil)
	assert.Equal(t, "custom system", activePrompts.System)
	assert.Equal(t, "custom vocab %s", activePrompts.VocabularyReference)
	assert.Equal(t, "custom append %s", activePrompts.AppendInputBuffer)
	assert.Equal(t, "custom append nv %s", activePrompts.AppendInputBufferNoVocab)
	assert.Equal(t, "custom edit %s", activePrompts.EditInputBuffer)
	assert.Equal(t, "custom task transcribe", activePrompts.TaskTranscribe)
	assert.Equal(t, "custom task transcribe vocab", activePrompts.TaskTranscribeWithVocab)
	assert.Equal(t, "custom task append", activePrompts.TaskAppend)
	assert.Equal(t, "custom task edit", activePrompts.TaskEdit)
}

func TestLoadPrompts_InvalidYAML(t *testing.T) {
	t.Cleanup(resetToDefaults)
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	os.WriteFile(path, []byte(`{{{invalid`), 0644)

	LoadPrompts(path, nil)
	assert.Equal(t, systemPrompt, activePrompts.System)
}

func TestLoadPrompts_EmptyFields(t *testing.T) {
	t.Cleanup(resetToDefaults)
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts.yaml")
	content := `
system: ""
vocabulary_reference: "custom vocab %s"
`
	os.WriteFile(path, []byte(content), 0644)

	LoadPrompts(path, nil)
	// Empty system should keep default
	assert.Equal(t, systemPrompt, activePrompts.System)
	// Non-empty vocabulary_reference should override
	assert.Equal(t, "custom vocab %s", activePrompts.VocabularyReference)
}

func TestLoadPrompts_MultilineBlockScalar(t *testing.T) {
	t.Cleanup(resetToDefaults)
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts.yaml")
	content := `system: |
  Line one.
  Line two.
  Line three.
`
	os.WriteFile(path, []byte(content), 0644)

	LoadPrompts(path, nil)
	assert.Equal(t, "Line one.\nLine two.\nLine three.", activePrompts.System)
}

func TestLoadPrompts_WhitespaceOnlyField(t *testing.T) {
	t.Cleanup(resetToDefaults)
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts.yaml")
	content := `
system: "   "
vocabulary_reference: "custom vocab %s"
`
	os.WriteFile(path, []byte(content), 0644)

	LoadPrompts(path, nil)
	// Whitespace-only system should keep default
	assert.Equal(t, systemPrompt, activePrompts.System)
	assert.Equal(t, "custom vocab %s", activePrompts.VocabularyReference)
}

func TestLoadPrompts_InvalidPlaceholderCount(t *testing.T) {
	t.Cleanup(resetToDefaults)
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts.yaml")

	content := `
vocabulary_reference: "no placeholder here"
append_input_buffer: "two %s placeholders %s"
edit_input_buffer: "missing placeholder"
`
	os.WriteFile(path, []byte(content), 0644)

	LoadPrompts(path, nil)
	// All should fall back to defaults due to wrong %s count
	assert.Equal(t, vocabularyReferenceTemplate, activePrompts.VocabularyReference)
	assert.Equal(t, appendInputBufferTemplate, activePrompts.AppendInputBuffer)
	assert.Equal(t, editInputBufferTemplate, activePrompts.EditInputBuffer)
}

func TestLoadPrompts_ValidPlaceholder(t *testing.T) {
	t.Cleanup(resetToDefaults)
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts.yaml")
	content := `
vocabulary_reference: "vocab %s list"
append_input_buffer: "append %s end"
append_input_buffer_no_vocab: "append nv %s end"
edit_input_buffer: "edit %s done"
`
	os.WriteFile(path, []byte(content), 0644)

	LoadPrompts(path, nil)
	assert.Equal(t, "vocab %s list", activePrompts.VocabularyReference)
	assert.Equal(t, "append %s end", activePrompts.AppendInputBuffer)
	assert.Equal(t, "append nv %s end", activePrompts.AppendInputBufferNoVocab)
	assert.Equal(t, "edit %s done", activePrompts.EditInputBuffer)
}

func TestLoadPrompts_LegacyFieldsIgnored(t *testing.T) {
	t.Cleanup(resetToDefaults)
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts.yaml")
	content := `
transcribe: "old transcribe prompt"
modify: "old modify %s"
append_context: "old append %s"
chat_context_suffix: "old suffix %s"
system: "new system prompt"
`
	os.WriteFile(path, []byte(content), 0644)

	LoadPrompts(path, nil)
	// New field should be applied
	assert.Equal(t, "new system prompt", activePrompts.System)
	// Legacy fields should not affect active prompts
	assert.Equal(t, vocabularyReferenceTemplate, activePrompts.VocabularyReference)
}

func TestLoadPrompts_TaskFieldsNoPlaceholderRequired(t *testing.T) {
	t.Cleanup(resetToDefaults)
	dir := t.TempDir()
	path := filepath.Join(dir, "prompts.yaml")
	content := `
task_transcribe: "transcribe task"
task_edit: "edit task with special %s chars"
`
	os.WriteFile(path, []byte(content), 0644)

	LoadPrompts(path, nil)
	// Task fields should accept any content (no %s validation)
	assert.Equal(t, "transcribe task", activePrompts.TaskTranscribe)
	assert.Equal(t, "edit task with special %s chars", activePrompts.TaskEdit)
}
