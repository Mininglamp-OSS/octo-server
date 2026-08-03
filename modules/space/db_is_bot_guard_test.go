package space

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// spaceMemberInsertRe 匹配任何往 space_member 插入行的 SQL 字面量。
var spaceMemberInsertRe = regexp.MustCompile(`(?i)INSERT\s+(IGNORE\s+)?INTO\s+` + "`?space_member`?")

// TestSpaceMemberInsertsPopulateIsBot 是一条跨模块的源码守卫：任何往
// space_member 写行的 SQL 都必须显式给出 is_bot。
//
// 为什么需要源码级守卫，而不是靠行为测试：space_member.is_bot 决定通讯录目录把
// 一行归为 AI 还是真人，列默认值是 0，迁移只回填存量。漏填的插入点不会报错、
// 不会告警，只会让此后新建的 bot 悄悄出现在「人类」tab 里，且移出再加回也修不好。
//
// 而 modules/space 自己的行为测试**结构上**发现不了它：fixture 走的是本包已经
// 正确填充的 insertMemberNoTx，永远拿不到错误的值。事实上首次实现就漏了
// modules/botfather 的三个插入点，正是被跨模块的 grep 而非测试发现的。
//
// 守卫扫描整个仓库而不只是本模块，因为写 space_member 的模块不止一个。
func TestSpaceMemberInsertsPopulateIsBot(t *testing.T) {
	root := "../.."
	var offenders []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// vendor / .git / node_modules 不是我们的源码
			switch info.Name() {
			case ".git", "vendor", "node_modules", ".octospec":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !spaceMemberInsertRe.MatchString(line) {
				continue
			}
			// dbr 的 Record 式插入按结构体字段生成列（MemberModel.IsBot 已覆盖），
			// 只有手写列清单的语句需要显式带上 is_bot。
			if strings.Contains(line, "InsertInto(") {
				continue
			}
			if !strings.Contains(line, "is_bot") {
				offenders = append(offenders, filepath_line(path, i+1, line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	if len(offenders) > 0 {
		t.Errorf("以下 space_member 插入语句未填 is_bot，新建的 bot 会被通讯录目录错分成真人：\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

func filepath_line(path string, line int, src string) string {
	return strings.TrimPrefix(path, "../../") + ":" + itoa(line) + "  " + strings.TrimSpace(src)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
