package space

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// space_member 的「移除」写法有两种：手写 SQL 与 dbr builder。两种都要覆盖，
// 否则守卫只会挡住它见过的那一种。
var (
	memberRemoveRawRe    = regexp.MustCompile("(?is)UPDATE\\s+`?space_member`?\\s+SET\\s+status\\s*=\\s*0")
	memberRemoveDBRRe    = regexp.MustCompile(`(?s)Update\("space_member"\)(?:[^\n]*\n){0,4}?[^\n]*Set\("status",\s*0\)`)
	memberRemoveDeleteRe = regexp.MustCompile("(?is)DELETE\\s+FROM\\s+`?space_member`?")
	lineCommentRe        = regexp.MustCompile(`(?m)//.*$`)
)

// revocationSites 登记每一个把成员移出 Space 的写入点，以及负责清除
// SpaceMiddleware 成员缓存的文件。
//
// key   = 执行移除写入的文件
// value = 执行缓存失效的文件（多数情况下是它的 handler 调用方，而不是 DB 层自己：
//
//	失效必须发生在事务提交之后，DB 层拿不到那个时点）
//
// 新增一处移除写入就必须在这里登记，并且被指向的文件确实要调用失效函数——守卫
// 会回去检查，光填个名字过不了。
var revocationSites = map[string]string{
	// removeMembersForce（管理端强制移除）与 forceDisbandSpace（管理端强制解散）
	"modules/space/db_manager.go": "modules/space/api_manager.go",
	// /deletebot 命令：把 bot 一次性移出全部 Space
	"modules/botfather/command.go": "modules/botfather/command.go",
	// DELETE /v1/user/bots/{id}：同上
	"modules/botfather/api_user.go": "modules/botfather/api_user.go",
}

// revocationCallRe 匹配任何一种失效调用。三个 wrapper 最终都落到
// pkg/space.InvalidateMembershipCache。
var revocationCallRe = regexp.MustCompile(`revokeMembershipCache\(|revokeMembershipCacheForSpace\(|revokeBotMembershipCache\(|InvalidateMembershipCache\(`)

// TestSpaceMemberRemovalsRevokeMembershipCache 是一条跨模块源码守卫：任何把成员
// 移出 Space 的写入点，都必须有一处配套的成员缓存失效。
//
// 为什么需要源码级守卫，而不是只靠行为测试：SpaceMiddleware 把「是成员」的判定
// 在 Redis 里缓存 60s，漏掉失效不会报错、不会告警、功能测试全绿——被移除的人只是
// 在接下来最多 60 秒里还能继续读这个 Space。GET /v1/space/{space_id}/directory
// 返回的是整份名册（uid + 展示名 + 角色 + 全部 bot），所以这段窗口漏的是 PII。
//
// 而针对某一条移除路径的行为测试**结构上**发现不了新增的另一条路径。这个仓库刚
// 因为同一个理由付出过代价：is_bot 的插入点清单是靠一次被 `head` 截断的 grep 得出
// 的，漏掉了 modules/botfather 的三处，而所有测试都是绿的。清单类的正确性要由守卫
// 保证，不能由某一次搜索的仔细程度保证。
//
// 守卫扫全仓库而不只是本模块：写 space_member 的模块不止一个。
func TestSpaceMemberRemovalsRevokeMembershipCache(t *testing.T) {
	const root = "../.."

	readSource := func(path string) (string, error) {
		data, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			return "", err
		}
		// 注释里引用 SQL（本仓库的注释密度下很常见）不该被当成写入点，
		// 也不该被当成失效调用。
		return lineCommentRe.ReplaceAllString(string(data), ""), nil
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
		src, readErr := readSource(rel)
		if readErr != nil {
			return readErr
		}
		if !memberRemoveRawRe.MatchString(src) &&
			!memberRemoveDBRRe.MatchString(src) &&
			!memberRemoveDeleteRe.MatchString(src) {
			return nil
		}
		if _, ok := revocationSites[rel]; !ok {
			unregistered = append(unregistered, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	if len(unregistered) > 0 {
		t.Errorf("以下文件把成员移出了 Space，但没有在 revocationSites 里登记缓存失效。\n"+
			"被移除的人会在 SpaceMiddleware 的成员缓存过期前（≤60s）继续读到该 Space 的数据，"+
			"包括 /v1/space/{space_id}/directory 的整份名册：\n  %s",
			strings.Join(unregistered, "\n  "))
	}

	// 登记表本身也要成立：被指向的文件必须真的调用失效，且写入点文件必须还在。
	for writer, revoker := range revocationSites {
		if _, statErr := os.Stat(filepath.Join(root, writer)); statErr != nil {
			t.Errorf("revocationSites 登记了不存在的写入点 %s：文件已删除或改名，请同步更新登记表", writer)
			continue
		}
		src, readErr := readSource(revoker)
		if readErr != nil {
			t.Errorf("revocationSites[%s] 指向的 %s 读不到：%v", writer, revoker, readErr)
			continue
		}
		if !revocationCallRe.MatchString(src) {
			t.Errorf("revocationSites[%s] 指向 %s，但该文件没有任何成员缓存失效调用。"+
				"登记表不是豁免清单——填了名字仍然要有真的失效代码", writer, revoker)
		}
	}
}
