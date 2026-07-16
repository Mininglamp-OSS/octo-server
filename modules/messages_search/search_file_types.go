package messages_search

import (
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	appwkhttp "github.com/Mininglamp-OSS/octo-server/pkg/wkhttp"
)

// FileTypeEntry is one row of the enum returned by
// GET /v1/messages/_search_file_types (§7.5).
type FileTypeEntry struct {
	Key   string   `json:"key"`
	Label string   `json:"label"`
	Exts  []string `json:"exts"`
}

// fileTypeEnum is the hardcoded enum surface. Must stay in sync with
// octo-web packages/dmworkbase/src/Utils/fileIcon.ts::getFileIconByExtension
// (§7.4). Update both sides together whenever a category or extension is
// added / removed. Order matters — mirrors the reference table so the
// frontend can render the dropdown in a stable, documented sequence without
// re-sorting.
var fileTypeEnum = []FileTypeEntry{
	{Key: "doc", Label: "文档", Exts: []string{"doc", "docx"}},
	{Key: "excel", Label: "表格", Exts: []string{"xls", "xlsx"}},
	{Key: "pdf", Label: "PDF", Exts: []string{"pdf"}},
	{Key: "archive", Label: "压缩包", Exts: []string{"zip", "rar", "7z", "tar", "gz"}},
	{Key: "html", Label: "网页", Exts: []string{"html", "htm"}},
	{Key: "txt", Label: "文本", Exts: []string{"txt"}},
	{Key: "md", Label: "Markdown", Exts: []string{"md"}},
	{Key: "gif", Label: "GIF", Exts: []string{"gif"}},
	{Key: "video", Label: "视频", Exts: []string{"mp4", "avi", "mov", "mkv", "webm"}},
}

// knownFileExts is the union of every extension in fileTypeEnum. Built once at
// init so validateGlobalFileBase can check filters.file_exts membership in
// O(1) without walking the enum on every request.
var knownFileExts = func() map[string]struct{} {
	out := make(map[string]struct{})
	for _, entry := range fileTypeEnum {
		for _, ext := range entry.Exts {
			out[ext] = struct{}{}
		}
	}
	return out
}()

func isKnownFileExt(ext string) bool {
	_, ok := knownFileExts[ext]
	return ok
}

// mountFileTypesRoute wires GET /v1/messages/_search_file_types on a route
// group that only carries AuthMiddleware (§7.5 explicit constraint —
// the /_search* group is off-limits because backendGate + SpaceMiddleware
// would incorrectly gate a static enum endpoint on the search backend being
// enabled and on the caller carrying a Space).
//
// Called from Route() in api.go alongside the search-group mount.
func (h *Handler) mountFileTypesRoute(r *wkhttp.WKHttp) {
	// SharedUIDRateLimiter is safe here even without SpaceMiddleware: it reads
	// the login UID off AuthMiddleware and buckets requests globally per uid,
	// matching the same protection layer the search endpoints get.
	g := r.Group("/v1/messages",
		h.ctx.AuthMiddleware(r),
		appwkhttp.SharedUIDRateLimiter(r, h.ctx),
	)
	g.GET("/_search_file_types", h.searchFileTypes)
}

// searchFileTypes returns the fileTypeEnum as a bare JSON array (§7.5). The
// enum is static so no ES/DB call is made and search_disabled deployments
// still receive it.
func (h *Handler) searchFileTypes(c *wkhttp.Context) {
	c.Response(fileTypeEnum)
}
