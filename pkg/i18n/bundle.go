// Package i18n 提供 octo-server 的国际化 SDK 入口。bundle.go 负责 go-i18n
// 的 bundle 单例初始化、locale TOML 加载、以及把 codes 注册表的 DefaultMessage
// 注入为 source 语言消息。
package i18n

import (
	"embed"
	"fmt"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"

	"github.com/Mininglamp-OSS/octo-server/pkg/i18n/codes"
)

// SourceLanguage 是 source（D4）语言标签——AST extractor 输出 / 翻译模板基准。
const SourceLanguage = "en-US"

// 仅 embed active.*.toml；translate.*.toml 是 goi18n merge 流程的 WIP 文件
// （含未翻译/部分翻译 stub），不应进入 runtime bundle，否则字典序后载会覆盖
// active.*.toml 让客户端看到 stub 文案。
//
//go:embed locales/active.*.toml
var localesFS embed.FS

var (
	bundleOnce sync.Once
	bundlePtr  *i18n.Bundle
	bundleErr  error
)

// Bundle 返回 lazy-init 的 go-i18n bundle 单例。
//
// 初始化顺序（关键）：
//  1. 创建 bundle，default language = SourceLanguage（en-US）。
//  2. 从 codes.All() 注入 DefaultMessage 为 source 语言消息——保证即使
//     active.en-US.toml 为空，所有已注册 code 都能 fallback 到 DefaultMessage。
//  3. 加载 locales/*.toml——同 ID 后加载者覆盖（即 TOML 可改写源消息，
//     翻译文件提供其他语言）。
//
// 初始化失败时 bundle 为 nil + bundleErr 非 nil；调用方（Localizer.Translate）
// 必须做 nil 检查并走 fallbackMessage 链路（D22）。Bundle 错误不 panic——
// 服务启动后才被调用，panic 会让运行中进程崩溃，违背「fail-soft 翻译」原则。
func Bundle() (*i18n.Bundle, error) {
	bundleOnce.Do(loadBundle)
	return bundlePtr, bundleErr
}

func loadBundle() {
	b := i18n.NewBundle(language.MustParse(SourceLanguage))
	b.RegisterUnmarshalFunc("toml", toml.Unmarshal)

	// Step 1: 注入 codes.Register 中的 DefaultMessage 为 source 语言消息。
	// 这是 source-of-truth 路径；TOML 文件存在仅为 AST extractor / 翻译团队
	// 协作所需，运行期不依赖 active.en-US.toml 内容。
	srcTag := language.MustParse(SourceLanguage)
	for _, c := range codes.All() {
		msg := &i18n.Message{ID: c.ID, Other: c.DefaultMessage}
		if err := b.AddMessages(srcTag, msg); err != nil {
			bundleErr = fmt.Errorf("i18n bundle: inject source for %q: %w", c.ID, err)
			return
		}
	}

	// Step 2: 加载 locales/*.toml。go-i18n 会按文件名解析 lang
	// （active.en-US.toml / translate.zh-CN.toml 等），覆盖同 ID 消息。
	entries, err := localesFS.ReadDir("locales")
	if err != nil {
		bundleErr = fmt.Errorf("i18n bundle: read locales dir: %w", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		data, err := localesFS.ReadFile("locales/" + e.Name())
		if err != nil {
			bundleErr = fmt.Errorf("i18n bundle: read %s: %w", e.Name(), err)
			return
		}
		// 空文件 / 仅注释的 TOML 在 go-i18n 内被解析为零消息，不报错；
		// 占位 active.en-US.toml 在此处不会触发任何副作用。
		if _, err := b.ParseMessageFileBytes(data, e.Name()); err != nil {
			bundleErr = fmt.Errorf("i18n bundle: parse %s: %w", e.Name(), err)
			return
		}
	}

	bundlePtr = b
}

// resetBundle 仅供测试使用，**非 goroutine 安全**——直接重置 sync.Once
// 与 bundlePtr / bundleErr 三个全局变量，调用方需保证无并发 Bundle()。
// 生产代码绝不应调用：bundle 是请求路径上的高频共享对象，运行期重建会引入
// 数据竞争与翻译瞬时不一致。
func resetBundle() {
	bundleOnce = sync.Once{}
	bundlePtr = nil
	bundleErr = nil
}
