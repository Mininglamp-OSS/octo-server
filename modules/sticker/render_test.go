package sticker

import (
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/stretchr/testify/assert"
)

// fakeURLResolver is a deterministic stickerURLResolver so renderablePath can be
// unit-tested without a real object-storage backend.
type fakeURLResolver struct {
	fn func(path, filename string) (string, error)
}

func (f fakeURLResolver) DownloadURL(path, filename string) (string, error) {
	return f.fn(path, filename)
}

// newRenderSticker builds a Sticker with just the logger + resolver wired — the
// only fields renderablePath touches — so the guard/fallback branches (which log
// a warning) do not nil-panic on the embedded logger.
func newRenderSticker(r stickerURLResolver) *Sticker {
	return &Sticker{Log: log.NewTLog("sticker-render-test"), fileURL: r}
}

func TestRenderablePath(t *testing.T) {
	const resolved = "https://cdn.example.com/sticker/uid/a.png"

	// resolver records the key it was asked to resolve so we can assert the
	// "file/preview/" prefix was stripped before resolution.
	var gotKey string
	ok := newRenderSticker(fakeURLResolver{fn: func(path, _ string) (string, error) {
		gotKey = path
		return resolved, nil
	}})

	t.Run("absolute http passes through untouched", func(t *testing.T) {
		gotKey = ""
		in := "http://cdn.example.com/x.png"
		assert.Equal(t, in, ok.renderablePath(in))
		assert.Empty(t, gotKey, "resolver must not be called for an absolute URL")
	})

	t.Run("absolute https passes through untouched", func(t *testing.T) {
		gotKey = ""
		in := "https://cdn.example.com/x.png"
		assert.Equal(t, in, ok.renderablePath(in))
		assert.Empty(t, gotKey, "resolver must not be called for an absolute URL")
	})

	t.Run("bare object key is resolved", func(t *testing.T) {
		gotKey = ""
		assert.Equal(t, resolved, ok.renderablePath("sticker/uid/a.png"))
		assert.Equal(t, "sticker/uid/a.png", gotKey)
	})

	t.Run("file/preview prefix is stripped before resolving", func(t *testing.T) {
		gotKey = ""
		assert.Equal(t, resolved, ok.renderablePath("file/preview/sticker/uid/a.png"))
		assert.Equal(t, "sticker/uid/a.png", gotKey,
			"leading file/preview/ must be stripped so legacy rows resolve like bare keys")
	})

	t.Run("empty stays empty", func(t *testing.T) {
		assert.Equal(t, "", ok.renderablePath(""))
	})

	t.Run("traversal key is not resolved (defense in depth)", func(t *testing.T) {
		gotKey = ""
		// A ".." segment must never reach DownloadURL: url.JoinPath would resolve
		// it and escape the sticker/ keyspace. renderablePath returns the stored
		// value unchanged (a broken but non-escaping render) instead.
		assert.Equal(t, "sticker/../a.png", ok.renderablePath("sticker/../a.png"))
		assert.Empty(t, gotKey, "resolver must not be called for a traversal key")
	})

	t.Run("traversal key via preview prefix is not resolved", func(t *testing.T) {
		gotKey = ""
		in := "file/preview/sticker/../a.png"
		assert.Equal(t, in, ok.renderablePath(in))
		assert.Empty(t, gotKey, "resolver must not be called for a traversal key")
	})

	t.Run("resolver error falls back to stored value", func(t *testing.T) {
		s := newRenderSticker(fakeURLResolver{fn: func(string, string) (string, error) {
			return "", errors.New("boom")
		}})
		assert.Equal(t, "sticker/uid/a.png", s.renderablePath("sticker/uid/a.png"))
	})

	t.Run("empty resolver result falls back to stored value", func(t *testing.T) {
		s := newRenderSticker(fakeURLResolver{fn: func(string, string) (string, error) {
			return "", nil
		}})
		assert.Equal(t, "sticker/uid/a.png", s.renderablePath("sticker/uid/a.png"))
	})

	t.Run("nil resolver falls back to stored value", func(t *testing.T) {
		s := newRenderSticker(nil)
		assert.Equal(t, "sticker/uid/a.png", s.renderablePath("sticker/uid/a.png"))
	})
}
