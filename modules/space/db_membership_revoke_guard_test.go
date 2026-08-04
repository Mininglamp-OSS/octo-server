package space

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// 能让 CheckMembership 翻成 false 的写入形态。它的 SQL 是
//
//	SELECT COUNT(*) FROM space_member sm
//	INNER JOIN space s ON s.space_id = sm.space_id AND s.status = 1
//	WHERE sm.uid = ? AND sm.space_id = ? AND sm.status = 1
//
// 所以「失去成员身份」有三条路：space_member.status 不再是 1、space_member 行被删、
// space.status 不再是 1。第三条最容易被漏——封禁（status=2）和解散（status=0）在这
// 个 JOIN 面前完全等价，而封禁一行 space_member 都不碰。
//
// 检测刻意放宽：宁可误报（登记一下即可）也不能漏报。值一律不做匹配——写成字面量 0、
// 写成 SpaceStatusBanned 这样的具名常量、或写成变量，在这里都算命中。
var (
	// space_member 上的 status 写入或删除
	memberStatusWriteRe = regexp.MustCompile(`(?is)UPDATE\s+` + "`?space_member`?" + `.*?\bstatus\b\s*=`)
	memberDBRStatusRe   = regexp.MustCompile(`(?s)Update\("space_member"\).*?Set\("status",`)
	memberDeleteRawRe   = regexp.MustCompile("(?is)DELETE\\s+FROM\\s+`?space_member`?")
	memberDeleteDBRRe   = regexp.MustCompile(`DeleteFrom\("space_member"\)`)
	// space 表自身的 status 写入（解散 / 封禁 / 解禁）
	spaceStatusWriteRe = regexp.MustCompile(`(?is)UPDATE\s+` + "`?space`?" + `\s+SET\b.*?\bstatus\b\s*=`)
	spaceDBRStatusRe   = regexp.MustCompile(`(?s)Update\("space"\).*?Set\("status",`)
)

func matchesMembershipWrite(src string) bool {
	for _, re := range []*regexp.Regexp{
		memberStatusWriteRe, memberDBRStatusRe, memberDeleteRawRe, memberDeleteDBRRe,
		spaceStatusWriteRe, spaceDBRStatusRe,
	} {
		if re.MatchString(src) {
			return true
		}
	}
	return false
}

// revocationSites 按 **函数** 登记，不是按文件。
//
// 按文件登记是上一版的致命缺陷：三个既有写入点分别住在 db_manager.go 与两个
// botfather 文件里，而这些文件已经在登记表中——于是「在最自然的地方新增一处移除」
// 恰好是守卫看不见的那种改法。函数粒度把这个缺口关上：新函数就是新条目。
//
// key   = "<相对路径>:<函数名>"，写入发生的地方
// value = "<相对路径>:<函数名>"，负责清缓存的地方
//
// 多数情况下二者不同：失效必须发生在事务提交之后，DB 层拿不到那个时点，所以由
// handler 负责。守卫会回去检查 value 指向的函数体里确实有失效调用——登记表不是
// 豁免清单，填个名字过不了。
//
// 授予成员身份的写入（status=1）同样会被检测命中，因为它们也写 status。它们登记在
// 这里并注明理由：加入路径留下的是**否定**缓存（30s），那是中间件设计时接受的加入
// 延迟，不是安全问题；一并列出比让检测去区分「加」和「减」更可靠——后者要判断写入的
// 值，而值可以是常量或变量，正是这次要摆脱的脆弱假设。
var revocationSites = map[string]string{
	// —— 撤权：必须清缓存 ——
	"modules/space/db_manager.go:removeMemberLocked": "modules/space/api.go:removeMembers",
	"modules/space/db_manager.go:removeMembersForce": "modules/space/api_manager.go:removeMembers",
	"modules/space/db_manager.go:forceDisbandSpace":  "modules/space/api_manager.go:forceDisband",
	"modules/space/db_manager.go:updateSpaceStatus":  "modules/space/api_manager.go:liftBan",
	"modules/space/db.go:disbandSpace":               "modules/space/api.go:disbandSpace",
	"modules/botfather/command.go:onDeleteConfirm":   "modules/botfather/command.go:onDeleteConfirm",
	"modules/botfather/api_user.go:deleteUserBot":    "modules/botfather/api_user.go:deleteUserBot",

	// —— 授予成员身份：写的是 status=1，留下的是**否定**缓存（30s）——
	// 那是中间件设计时就接受的加入延迟（negativeCacheTTL 的注释写着「新成员加入后
	// 快速生效」），不是撤权，也不是安全问题，所以 revoker 留空。
	//
	// 它们出现在这张表里是检测刻意放宽的结果：检测只看「写了 status」而不看写进去的
	// 值，因为值可以是字面量、具名常量或变量——靠判断值来区分加减，正是上一版那种
	// 会漏报的脆弱假设。宁可让加入路径在这里登记一行。
	"modules/space/db.go:reactivateMember":                "",
	"modules/space/db.go:approveJoinApplyAtomic":          "",
	"modules/space/db.go:atomicReactivateMemberIfNotFull": "",
	"modules/space/db_manager.go:upsertMembers":           "",
}

// revocationCallRe 匹配任何一种失效调用。三个 wrapper 最终都落到
// pkg/space.InvalidateMembershipCache。
var revocationCallRe = regexp.MustCompile(`revokeMembershipCache\(|revokeMembershipCacheForSpace\(|revokeBotMembershipCache\(|InvalidateMembershipCache\(`)

// funcBodies 解析一个 Go 文件，返回 函数名 -> 不含注释的函数源码。
//
// 用 AST 而不是逐行正则有两个理由：一是能把「写入点」定位到函数而不是文件（见
// revocationSites）；二是 printer 输出天然不含注释，不必再拿正则去剥注释——上一版
// 那种 `//.*$` 剥法会连字符串字面量里的 // 一起吃掉。
func funcBodies(path string) (map[string]string, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0) // 不带 ParseComments：注释不进 AST
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, fn); err != nil {
			return nil, err
		}
		out[fn.Name.Name] = buf.String()
	}
	return out, nil
}

// TestSpaceMemberRemovalsRevokeMembershipCache 是一条跨模块源码守卫：任何能让
// CheckMembership 翻成 false 的写入，都必须有一处配套的成员缓存失效。
//
// 为什么需要源码级守卫，而不是只靠行为测试：SpaceMiddleware 把「是成员」的判定在
// Redis 里缓存 60s，漏掉失效不会报错、不会告警、功能测试全绿——被移除的人只是在接
// 下来最多 60 秒里还能继续读这个 Space。GET /v1/space/{space_id}/directory 返回的
// 是整份名册（uid + 展示名 + 角色 + 全部 bot），所以这段窗口漏的是 PII。
//
// 针对某一条移除路径的行为测试**结构上**发现不了新增的另一条路径。这个仓库已经为
// 「清单靠一次搜索得出」付过两次代价：is_bot 的插入点清单被 `head` 截断漏了三处；
// 本守卫的第一版按文件登记，导致「在已登记文件里新增一处移除」完全不可见，而封禁
// 路径（space.status=2）从一开始就不在检测范围内。两次都是评审发现的，不是测试。
func TestSpaceMemberRemovalsRevokeMembershipCache(t *testing.T) {
	const root = "../.."

	bodiesCache := map[string]map[string]string{}
	bodiesOf := func(rel string) (map[string]string, error) {
		if b, ok := bodiesCache[rel]; ok {
			return b, nil
		}
		b, err := funcBodies(filepath.Join(root, rel))
		if err != nil {
			return nil, err
		}
		bodiesCache[rel] = b
		return b, nil
	}

	var unregistered []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "vendor", "node_modules", ".octospec":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		bodies, parseErr := bodiesOf(rel)
		if parseErr != nil {
			return parseErr
		}
		for name, src := range bodies {
			if !matchesMembershipWrite(src) {
				continue
			}
			if _, ok := revocationSites[rel+":"+name]; !ok {
				unregistered = append(unregistered, rel+":"+name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	if len(unregistered) > 0 {
		sort.Strings(unregistered)
		t.Errorf("以下函数写了 space_member.status / space.status，或删了 space_member 行，"+
			"但没有在 revocationSites 里登记。\n"+
			"任何让 CheckMembership 翻成 false 的写入都必须配套清除成员缓存，否则被移除的人"+
			"会在缓存过期前（≤60s）继续读到该 Space 的数据，包括 "+
			"/v1/space/{space_id}/directory 的整份名册。\n"+
			"若该函数只是授予成员身份（status=1），登记时把 revoker 留空并说明理由：\n  %s",
			strings.Join(unregistered, "\n  "))
	}

	// 登记表本身也要成立：写入点函数必须仍然存在，被指向的失效函数也必须存在且
	// 确实调用了失效。否则登记表会随着重构悄悄退化成一张过期的豁免清单。
	for site, revoker := range revocationSites {
		siteFile, siteFunc, _ := strings.Cut(site, ":")
		bodies, readErr := bodiesOf(siteFile)
		if readErr != nil {
			t.Errorf("revocationSites 登记的写入点 %s 所在文件读不到：%v（文件已删除或改名？）", site, readErr)
			continue
		}
		if _, ok := bodies[siteFunc]; !ok {
			t.Errorf("revocationSites 登记了不存在的函数 %s：已删除或改名，请同步更新登记表", site)
			continue
		}
		if revoker == "" {
			continue // 授予路径，无需失效
		}
		revFile, revFunc, _ := strings.Cut(revoker, ":")
		revBodies, readErr := bodiesOf(revFile)
		if readErr != nil {
			t.Errorf("revocationSites[%s] 指向的 %s 读不到：%v", site, revoker, readErr)
			continue
		}
		body, ok := revBodies[revFunc]
		if !ok {
			t.Errorf("revocationSites[%s] 指向不存在的函数 %s", site, revoker)
			continue
		}
		if !revocationCallRe.MatchString(body) {
			t.Errorf("revocationSites[%s] 指向 %s，但该函数体里没有任何成员缓存失效调用。"+
				"登记表不是豁免清单——填了名字仍然要有真的失效代码", site, revoker)
		}
	}
}

// TestRemoveMembersInvalidatesOnEveryExit 钉住 removeMembers 的失效调用必须在
// defer 里，而不是循环之后的尾调用。
//
// 为什么是源码断言而不是行为测试：removeMemberLocked 每个 uid 一个事务、逐个提交，
// 所以「批量中途失败」时前几个 uid 已经作为已移除落库，而写在循环之后的失效会被
// return 整个跳过——写入成功、撤权丢失。要在测试里确定性地触发这个分支，需要让
// 第 k 个 uid 的 DB 操作真的报错：评审复现时是从另一条连接锁住目标行、再把
// innodb_lock_wait_timeout 调到 2s 做到的，而 innodb_lock_wait_timeout 的 GLOBAL
// 值只对新会话生效，handler 用的是连接池里的旧连接，在测试里不可靠。
//
// 所以这里退一步，断言那个让这类失败不可能发生的**结构**：只要调用在 defer 里，
// 函数从哪条路径退出都会执行。这也正好挡住最可能的回退——有人觉得 defer 多余，
// 「简化」成循环后的直调。
func TestRemoveMembersInvalidatesOnEveryExit(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "api.go", nil, 0)
	if err != nil {
		t.Fatalf("parse api.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, decl := range file.Decls {
		if d, ok := decl.(*ast.FuncDecl); ok && d.Name.Name == "removeMembers" {
			fn = d
			break
		}
	}
	if fn == nil {
		t.Fatal("modules/space/api.go 里找不到 removeMembers")
	}

	var deferred, plain int
	ast.Inspect(fn, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, fset, d); err != nil {
			t.Fatalf("print defer: %v", err)
		}
		if revocationCallRe.MatchString(buf.String()) {
			deferred++
		}
		return true
	})

	var whole bytes.Buffer
	if err := printer.Fprint(&whole, fset, fn); err != nil {
		t.Fatalf("print func: %v", err)
	}
	plain = len(revocationCallRe.FindAllString(whole.String(), -1)) - deferred

	if deferred == 0 {
		t.Error("removeMembers 的成员缓存失效必须写在 defer 里：" +
			"removeMemberLocked 逐 uid 提交，循环中途出错会 return，" +
			"写在循环之后的失效会被跳过，已经落库为已移除的那几个人的正向缓存原样留着")
	}
	if plain > 0 {
		t.Errorf("removeMembers 里还有 %d 处非 defer 的失效调用。"+
			"留着尾调用等于把同一个缺陷再引入一次——所有退出路径都必须经过 defer", plain)
	}
}
