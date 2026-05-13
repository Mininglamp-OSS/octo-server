package message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/stretchr/testify/assert"
)

func TestCategoryFromFilename(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		expected fileCategory
	}{
		// document
		{"pdf", "report.pdf", fileCategoryDocument},
		{"docx", "合同.docx", fileCategoryDocument},
		{"xlsx", "data.xlsx", fileCategoryDocument},
		{"pptx", "slides.pptx", fileCategoryDocument},
		{"txt", "notes.txt", fileCategoryDocument},
		{"md", "README.md", fileCategoryDocument},
		{"csv", "export.csv", fileCategoryDocument},

		// archive
		{"zip", "backup.zip", fileCategoryArchive},
		{"rar", "files.rar", fileCategoryArchive},
		{"7z", "data.7z", fileCategoryArchive},
		{"tar", "release.tar", fileCategoryArchive},
		{"gz", "logs.gz", fileCategoryArchive},
		{"tgz", "package.tgz", fileCategoryArchive},

		// code
		{"json", "config.json", fileCategoryCode},
		{"yaml", "deploy.yaml", fileCategoryCode},
		{"go", "main.go", fileCategoryCode},
		{"py", "script.py", fileCategoryCode},
		{"js", "app.js", fileCategoryCode},
		{"sql", "migration.sql", fileCategoryCode},
		{"proto", "service.proto", fileCategoryCode},

		// other (unknown extensions)
		{"exe", "setup.exe", fileCategoryOther},
		{"apk", "app.apk", fileCategoryOther},
		{"dmg", "installer.dmg", fileCategoryOther},
		{"iso", "ubuntu.iso", fileCategoryOther},

		// edge cases
		{"no extension", "Makefile", fileCategoryOther},
		{"uppercase ext", "DATA.PDF", fileCategoryDocument},
		{"mixed case", "Report.Docx", fileCategoryDocument},
		{"empty string", "", fileCategoryOther},
		{"dot only", ".", fileCategoryOther},
		{"double ext", "archive.tar.gz", fileCategoryArchive},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := categoryFromFilename(tt.filename)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestPayloadTypesForCategory(t *testing.T) {
	tests := []struct {
		category fileCategory
		expected []int
	}{
		{fileCategoryAll, []int{common.Image.Int(), common.GIF.Int(), common.Video.Int(), common.File.Int()}},
		{fileCategoryImage, []int{common.Image.Int(), common.GIF.Int()}},
		{fileCategoryVideo, []int{common.Video.Int()}},
		{fileCategoryDocument, []int{common.File.Int()}},
		{fileCategoryArchive, []int{common.File.Int()}},
		{fileCategoryCode, []int{common.File.Int()}},
	}
	for _, tt := range tests {
		t.Run(string(tt.category), func(t *testing.T) {
			got := payloadTypesForCategory(tt.category)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestNeedsExtFilter(t *testing.T) {
	assert.True(t, needsExtFilter(fileCategoryDocument))
	assert.True(t, needsExtFilter(fileCategoryArchive))
	assert.True(t, needsExtFilter(fileCategoryCode))
	assert.False(t, needsExtFilter(fileCategoryAll))
	assert.False(t, needsExtFilter(fileCategoryImage))
	assert.False(t, needsExtFilter(fileCategoryVideo))
}

func TestPayloadInt64_JsonNumber(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		key      string
		expected int64
	}{
		{"json.Number integer", `{"size": 12345}`, "size", 12345},
		{"json.Number zero", `{"size": 0}`, "size", 0},
		{"json.Number large", `{"size": 104857600}`, "size", 104857600},
		{"missing key", `{"other": 1}`, "size", 0},
		{"null value", `{"size": null}`, "size", 0},
		{"string value", `{"size": "abc"}`, "size", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m map[string]interface{}
			decoder := json.NewDecoder(strings.NewReader(tt.json))
			decoder.UseNumber()
			err := decoder.Decode(&m)
			assert.NoError(t, err)
			got := payloadInt64(m, tt.key)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestPayloadInt64_Float64(t *testing.T) {
	var m map[string]interface{}
	err := json.Unmarshal([]byte(`{"size": 12345}`), &m)
	assert.NoError(t, err)
	got := payloadInt64(m, "size")
	assert.Equal(t, int64(12345), got)
}

func TestPayloadStr(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		key      string
		expected string
	}{
		{"normal string", `{"name": "report.pdf"}`, "name", "report.pdf"},
		{"empty string", `{"name": ""}`, "name", ""},
		{"missing key", `{"other": "x"}`, "name", ""},
		{"null value", `{"name": null}`, "name", ""},
		{"number value", `{"name": 123}`, "name", ""},
		{"chinese name", `{"name": "部署说明.md"}`, "name", "部署说明.md"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var m map[string]interface{}
			decoder := json.NewDecoder(strings.NewReader(tt.json))
			decoder.UseNumber()
			err := decoder.Decode(&m)
			assert.NoError(t, err)
			got := payloadStr(m, tt.key)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestFilenameFromURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected string
	}{
		{"simple", "https://cdn.example.com/chat/report.pdf", "report.pdf"},
		{"with query", "https://cdn.example.com/chat/report.pdf?token=abc", "report.pdf"},
		{"encoded chinese", "https://cdn.example.com/chat/%E6%8A%A5%E5%91%8A.pdf", "报告.pdf"},
		{"encoded space", "https://cdn.example.com/chat/hello%20world.pdf", "hello world.pdf"},
		{"empty", "", ""},
		{"no path", "https://cdn.example.com/", ""},
		{"trailing slash", "https://cdn.example.com/path/", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filenameFromURL(tt.url)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestCategoryForContentType(t *testing.T) {
	tests := []struct {
		name        string
		contentType int
		filename    string
		expected    fileCategory
	}{
		{"image", common.Image.Int(), "photo.jpg", fileCategoryImage},
		{"gif", common.GIF.Int(), "anim.gif", fileCategoryImage},
		{"video", common.Video.Int(), "clip.mp4", fileCategoryVideo},
		{"file doc", common.File.Int(), "report.pdf", fileCategoryDocument},
		{"file archive", common.File.Int(), "backup.zip", fileCategoryArchive},
		{"file code", common.File.Int(), "main.go", fileCategoryCode},
		{"file other", common.File.Int(), "app.exe", fileCategoryOther},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := categoryForContentType(tt.contentType, tt.filename)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestBuildChannelFileResp_File(t *testing.T) {
	msg := &config.MessageResp{
		MessageID:   123,
		MessageSeq:  10,
		FromUID:     "user1",
		ChannelID:   "group1",
		ChannelType: common.ChannelTypeGroup.Uint8(),
		Timestamp:   1714200000,
		Payload:     []byte(`{"type": 8, "url": "https://cdn.example.com/chat/report.pdf", "name": "report.pdf", "size": 12400}`),
	}

	resp := buildChannelFileResp(msg, common.File.Int())
	assert.NotNil(t, resp)
	assert.Equal(t, int64(123), resp.MessageID)
	assert.Equal(t, "user1", resp.FromUID)
	assert.Equal(t, "report.pdf", resp.Name)
	assert.Equal(t, "https://cdn.example.com/chat/report.pdf", resp.URL)
	assert.Equal(t, int64(12400), resp.Size)
	assert.Equal(t, int32(1714200000), resp.Timestamp)
}

func TestBuildChannelFileResp_Image(t *testing.T) {
	msg := &config.MessageResp{
		MessageID: 456,
		FromUID:   "user2",
		Timestamp: 1714200000,
		Payload:   []byte(`{"type": 2, "url": "https://cdn.example.com/chat/photo.jpg", "width": 1920, "height": 1080}`),
	}

	resp := buildChannelFileResp(msg, common.Image.Int())
	assert.NotNil(t, resp)
	assert.Equal(t, "photo.jpg", resp.Name)
	assert.Equal(t, 1920, resp.Width)
	assert.Equal(t, 1080, resp.Height)
	assert.Equal(t, int64(0), resp.Size)
}

func TestBuildChannelFileResp_Video(t *testing.T) {
	msg := &config.MessageResp{
		MessageID: 789,
		FromUID:   "user3",
		Payload:   []byte(`{"type": 5, "url": "https://cdn.example.com/chat/clip.mp4", "width": 1280, "height": 720, "duration": 30}`),
	}

	resp := buildChannelFileResp(msg, common.Video.Int())
	assert.NotNil(t, resp)
	assert.Equal(t, "clip.mp4", resp.Name)
	assert.Equal(t, 1280, resp.Width)
	assert.Equal(t, 720, resp.Height)
	assert.Equal(t, 30, resp.Duration)
}

func TestBuildChannelFileResp_NilPayload(t *testing.T) {
	msg := &config.MessageResp{
		MessageID: 100,
		FromUID:   "user1",
		Payload:   nil,
	}
	resp := buildChannelFileResp(msg, common.File.Int())
	assert.Nil(t, resp)
}

func TestBuildChannelFileResp_EmptyURL(t *testing.T) {
	msg := &config.MessageResp{
		MessageID: 100,
		FromUID:   "user1",
		Payload:   []byte(`{"type": 8, "url": "", "name": "nourl.pdf", "size": 100}`),
	}
	resp := buildChannelFileResp(msg, common.File.Int())
	assert.Nil(t, resp)
}

func TestBuildChannelFileResp_UnsupportedType(t *testing.T) {
	msg := &config.MessageResp{
		MessageID: 100,
		FromUID:   "user1",
		Payload:   []byte(`{"type": 1, "content": "hello"}`),
	}
	resp := buildChannelFileResp(msg, common.Text.Int())
	assert.Nil(t, resp)
}

func TestChannelFilesReq_Check(t *testing.T) {
	tests := []struct {
		name    string
		req     channelFilesReq
		wantErr bool
	}{
		{"valid", channelFilesReq{ChannelID: "group1", ChannelType: 2}, false},
		{"empty channel_id", channelFilesReq{ChannelID: "", ChannelType: 2}, true},
		{"spaces only channel_id", channelFilesReq{ChannelID: "   ", ChannelType: 2}, true},
		{"zero channel_type", channelFilesReq{ChannelID: "group1", ChannelType: 0}, true},
		{"invalid category", channelFilesReq{ChannelID: "group1", ChannelType: 2, Category: "hacker"}, true},
		{"valid category", channelFilesReq{ChannelID: "group1", ChannelType: 2, Category: "document"}, false},
		{"empty category is ok", channelFilesReq{ChannelID: "group1", ChannelType: 2, Category: ""}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.req.check()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// --- Handler tests ---

func mockWuKongIMServer(t *testing.T, messages []map[string]interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/plugins/wk.plugin.search/usersearch" {
			var reqBody struct {
				PayloadTypes []int `json:"payload_types"`
				Page         int   `json:"page"`
				Limit        int   `json:"limit"`
			}
			json.NewDecoder(r.Body).Decode(&reqBody)

			filtered := messages
			if len(reqBody.PayloadTypes) > 0 {
				typeSet := make(map[int]bool)
				for _, pt := range reqBody.PayloadTypes {
					typeSet[pt] = true
				}
				filtered = make([]map[string]interface{}, 0)
				for _, m := range messages {
					payloadBytes, _ := m["payload"].([]byte)
					var p map[string]interface{}
					json.Unmarshal(payloadBytes, &p)
					if pt, ok := p["type"].(float64); ok && typeSet[int(pt)] {
						filtered = append(filtered, m)
					}
				}
			}

			total := int64(len(filtered))
			if reqBody.Limit > 0 {
				start := (reqBody.Page - 1) * reqBody.Limit
				if start < 0 {
					start = 0
				}
				if start >= len(filtered) {
					filtered = nil
				} else {
					end := start + reqBody.Limit
					if end > len(filtered) {
						end = len(filtered)
					}
					filtered = filtered[start:end]
				}
			}

			resp := map[string]interface{}{
				"total":    total,
				"limit":    reqBody.Limit,
				"page":     reqBody.Page,
				"messages": filtered,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func makeMessageJSON(msgID int64, seq uint32, fromUID, channelID string, channelType uint8, payload map[string]interface{}) map[string]interface{} {
	payloadBytes, _ := json.Marshal(payload)
	return map[string]interface{}{
		"message_id":    msgID,
		"message_idstr": fmt.Sprintf("%d", msgID),
		"message_seq":   seq,
		"from_uid":      fromUID,
		"channel_id":    channelID,
		"channel_type":  channelType,
		"timestamp":     1714200000,
		"payload":       payloadBytes,
	}
}

func TestChannelFiles_InvalidParams(t *testing.T) {
	s, ctx := newTestServer()
	msg := New(ctx)
	msg.Route(s.GetRoute())

	tests := []struct {
		name string
		body map[string]interface{}
	}{
		{"missing channel_id", map[string]interface{}{"channel_type": 2}},
		{"empty channel_id", map[string]interface{}{"channel_id": "", "channel_type": 2}},
		{"missing channel_type", map[string]interface{}{"channel_id": "group1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("POST", "/v1/message/channel/files",
				bytes.NewReader([]byte(util.ToJson(tt.body))))
			req.Header.Set("token", token)
			s.GetRoute().ServeHTTP(w, req)
			assert.Equal(t, http.StatusBadRequest, w.Code)
		})
	}
}

func TestChannelFiles_EmptyResult(t *testing.T) {
	t.Skip("OCTO migration TODO: see https://github.com/Mininglamp-OSS/octo-server/issues/17")
	mockIM := mockWuKongIMServer(t, []map[string]interface{}{})
	defer mockIM.Close()

	s, ctx := newTestServer()
	ctx.GetConfig().WuKongIM.APIURL = mockIM.URL
	msg := New(ctx)
	msg.Route(s.GetRoute())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/v1/message/channel/files",
		bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
			"channel_id":   uid,
			"channel_type": 1,
			"category":     "all",
		}))))
	req.Header.Set("token", token)
	s.GetRoute().ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp channelFilesResp
	json.NewDecoder(w.Body).Decode(&resp)
	assert.Equal(t, int64(0), resp.Total)
	assert.Equal(t, 0, len(resp.Files))
}

func TestChannelFiles_WithFiles(t *testing.T) {
	messages := []map[string]interface{}{
		makeMessageJSON(1001, 10, "user1", uid, 1, map[string]interface{}{
			"type": 8, "url": "https://cdn.example.com/report.pdf", "name": "report.pdf", "size": 12400,
		}),
		makeMessageJSON(1002, 11, "user2", uid, 1, map[string]interface{}{
			"type": 2, "url": "https://cdn.example.com/photo.jpg", "width": 1920, "height": 1080,
		}),
		makeMessageJSON(1003, 12, "user1", uid, 1, map[string]interface{}{
			"type": 8, "url": "https://cdn.example.com/backup.zip", "name": "backup.zip", "size": 50000,
		}),
		makeMessageJSON(1004, 13, "user1", uid, 1, map[string]interface{}{
			"type": 8, "url": "https://cdn.example.com/main.go", "name": "main.go", "size": 2048,
		}),
		makeMessageJSON(1005, 14, "user2", uid, 1, map[string]interface{}{
			"type": 5, "url": "https://cdn.example.com/clip.mp4", "width": 1280, "height": 720, "duration": 30,
		}),
	}

	mockIM := mockWuKongIMServer(t, messages)
	defer mockIM.Close()

	s, ctx := newTestServer()
	ctx.GetConfig().WuKongIM.APIURL = mockIM.URL

	// handler 中查询 message_extra 等需要表存在
	for _, ddl := range []string{
		"CREATE TABLE IF NOT EXISTS message_extra (id INTEGER PRIMARY KEY AUTO_INCREMENT, message_id VARCHAR(40), channel_id VARCHAR(100), channel_type SMALLINT DEFAULT 0, `revoke` INT DEFAULT 0, revoker VARCHAR(40) DEFAULT '', is_deleted INT DEFAULT 0, is_mutual_deleted INT DEFAULT 0, readed_count INT DEFAULT 0, content_edit TEXT, content_edit_hash VARCHAR(40) DEFAULT '', edited_at INT DEFAULT 0, is_pinned INT DEFAULT 0, version BIGINT DEFAULT 0)",
		"CREATE TABLE IF NOT EXISTS message_user_extra (id INTEGER PRIMARY KEY AUTO_INCREMENT, uid VARCHAR(40), message_id VARCHAR(40), channel_id VARCHAR(100), channel_type SMALLINT DEFAULT 0, message_seq BIGINT DEFAULT 0, message_is_deleted INT DEFAULT 0, voice_readed INT DEFAULT 0)",
		"CREATE TABLE IF NOT EXISTS channel_offset (id INTEGER PRIMARY KEY AUTO_INCREMENT, uid VARCHAR(40), channel_id VARCHAR(100), channel_type SMALLINT DEFAULT 0, message_seq BIGINT DEFAULT 0)",
	} {
		_, err := ctx.DB().UpdateBySql(ddl).Exec()
		if err != nil {
			t.Fatalf("create table failed: %v", err)
		}
	}

	msg := New(ctx)
	msg.Route(s.GetRoute())

	doReq := func(t *testing.T, category string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("POST", "/v1/message/channel/files",
			bytes.NewReader([]byte(util.ToJson(map[string]interface{}{
				"channel_id":   uid,
				"channel_type": 1,
				"category":     category,
			}))))
		req.Header.Set("token", token)
		s.GetRoute().ServeHTTP(w, req)
		return w
	}

	t.Run("category=all returns all files", func(t *testing.T) {
		w := doReq(t, "all")
		assert.Equal(t, http.StatusOK, w.Code)
		t.Logf("response body: %s", w.Body.String())

		var resp channelFilesResp
		json.NewDecoder(w.Body).Decode(&resp)
		assert.Equal(t, 5, len(resp.Files))
		assert.Equal(t, int64(5), resp.Total)
	})

	t.Run("category=document filters by extension", func(t *testing.T) {
		w := doReq(t, "document")
		assert.Equal(t, http.StatusOK, w.Code)

		var resp channelFilesResp
		json.NewDecoder(w.Body).Decode(&resp)
		names := make([]string, 0)
		for _, f := range resp.Files {
			names = append(names, f.Name)
			assert.Equal(t, string(fileCategoryDocument), f.Category)
		}
		assert.Contains(t, names, "report.pdf")
		assert.NotContains(t, names, "backup.zip")
		assert.NotContains(t, names, "main.go")
	})

	t.Run("category=archive filters by extension", func(t *testing.T) {
		w := doReq(t, "archive")
		assert.Equal(t, http.StatusOK, w.Code)

		var resp channelFilesResp
		json.NewDecoder(w.Body).Decode(&resp)
		names := make([]string, 0)
		for _, f := range resp.Files {
			names = append(names, f.Name)
		}
		assert.Contains(t, names, "backup.zip")
		assert.NotContains(t, names, "report.pdf")
	})

	t.Run("category=code filters by extension", func(t *testing.T) {
		w := doReq(t, "code")
		assert.Equal(t, http.StatusOK, w.Code)

		var resp channelFilesResp
		json.NewDecoder(w.Body).Decode(&resp)
		names := make([]string, 0)
		for _, f := range resp.Files {
			names = append(names, f.Name)
		}
		assert.Contains(t, names, "main.go")
		assert.NotContains(t, names, "report.pdf")
	})

	t.Run("response fields are populated correctly", func(t *testing.T) {
		w := doReq(t, "all")

		var resp channelFilesResp
		json.NewDecoder(w.Body).Decode(&resp)

		for _, f := range resp.Files {
			if f.Name == "report.pdf" {
				assert.Equal(t, int64(12400), f.Size)
				assert.Equal(t, "user1", f.FromUID)
				assert.Equal(t, uid, f.ChannelID)
				assert.Equal(t, int32(1714200000), f.Timestamp)
				assert.Equal(t, string(fileCategoryDocument), f.Category)
				return
			}
		}
		t.Fatal("report.pdf not found in response")
	})

	t.Run("video has width/height/duration", func(t *testing.T) {
		w := doReq(t, "video")
		assert.Equal(t, http.StatusOK, w.Code)

		var resp channelFilesResp
		json.NewDecoder(w.Body).Decode(&resp)
		if assert.Equal(t, 1, len(resp.Files)) {
			assert.Equal(t, 1280, resp.Files[0].Width)
			assert.Equal(t, 720, resp.Files[0].Height)
			assert.Equal(t, 30, resp.Files[0].Duration)
		}
	})
}
