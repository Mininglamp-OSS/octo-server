// Package robot · 图文混排 RichText(=14) 发送校验单测。
//
// 任务背景：payloadIsVail 对 type=14 原本只检查 content != nil，伪造 / 残缺的
// RichText payload（如空 content 数组、image 缺 url / scheme 非法 / 缺尺寸）会被放行。
// 这里锁定升级后调 common.ValidateRichTextPayload 的 write-strict 行为。
package robot

import (
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/gookit/goutil/maputil"
	"github.com/stretchr/testify/assert"
)

func richTextData(content []interface{}) maputil.Data {
	m := maputil.Data{"type": common.RichText.Int()}
	if content != nil {
		m["content"] = content
	}
	return m
}

func TestPayloadIsVail_RichText(t *testing.T) {
	rb := &Robot{}

	cases := []struct {
		name    string
		payload maputil.Data
		want    bool
	}{
		{
			name: "valid_text_and_image",
			payload: richTextData([]interface{}{
				map[string]interface{}{"type": "text", "text": "hi"},
				map[string]interface{}{"type": "image", "url": "https://x/y.png", "width": 10, "height": 10},
			}),
			want: true,
		},
		{
			name:    "missing_content_rejected",
			payload: richTextData(nil),
			want:    false,
		},
		{
			name:    "empty_content_array_rejected",
			payload: richTextData([]interface{}{}),
			want:    false,
		},
		{
			name: "text_block_empty_rejected",
			payload: richTextData([]interface{}{
				map[string]interface{}{"type": "text", "text": "   "},
			}),
			want: false,
		},
		{
			name: "image_missing_url_rejected",
			payload: richTextData([]interface{}{
				map[string]interface{}{"type": "image", "width": 10, "height": 10},
			}),
			want: false,
		},
		{
			name: "image_bad_scheme_rejected",
			payload: richTextData([]interface{}{
				map[string]interface{}{"type": "image", "url": "data:image/png;base64,AAAA", "width": 10, "height": 10},
			}),
			want: false,
		},
		{
			name: "image_no_size_rejected",
			payload: richTextData([]interface{}{
				map[string]interface{}{"type": "image", "url": "https://x/y.png"},
			}),
			want: false,
		},
		{
			name: "unknown_block_rejected",
			payload: richTextData([]interface{}{
				map[string]interface{}{"type": "video", "url": "https://x/y.mp4"},
			}),
			want: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rb.payloadIsVail(c.payload)
			assert.Equal(t, c.want, got)
		})
	}
}
