package oidc

import "testing"

// 上游给的 phone_number 形态不受我们控制:厂商手册的 userinfo 示例是一个**不带
// 国家码的裸号**,而旧实现只认 "+86" 前缀,那种形态会被整条丢掉。
//
// 归一化的硬约束(每条都有对应用例):
//  1. 只在能**确定**号码归属地时才归一化。猜错国家码 = 号码写错 = 用户拿不回账号,
//     所以拿不准就返回空,让上层按"没有手机号"处理。
//  2. extractZone 与 extractPhone 必须同进同退。bind_service.go 用
//     `extractPhone(...) != ""` 判定手机可用性,而 UIDsByPhone 拿的是两个值 ——
//     一个有值一个空会查出错误的结果集。
func TestNormalizePhone(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		wantZone  string
		wantPhone string
	}{
		// 旧实现已支持的形态,不能回退
		{"e164 plus 86", "+8613812345678", "0086", "13812345678"},

		// 新支持:手册 userinfo 示例就是裸号形态
		{"bare mainland mobile", "13812345678", "0086", "13812345678"},
		{"zone prefix 0086", "008613812345678", "0086", "13812345678"},
		{"country code without plus", "8613812345678", "0086", "13812345678"},

		// 人工录入的分隔符
		{"with spaces", " +86 138 1234 5678 ", "0086", "13812345678"},
		{"with dashes", "+86-138-1234-5678", "0086", "13812345678"},

		// 拿不准的一律丢弃,不猜
		{"empty", "", "", ""},
		{"blank", "   ", "", ""},
		{"手册示例中的假号(111 开头不是合法号段)", "11136618971", "", ""},
		{"too short", "1381234567", "", ""},
		{"too long", "138123456789", "", ""},
		{"landline", "01012345678", "", ""},
		{"non-mainland country code", "+46701234567", "", ""},
		{"plus 86 but body invalid", "+8611136618971", "", ""},
		{"not a number at all", "not-a-phone", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			zone, phone := normalizePhone(c.in)
			if zone != c.wantZone || phone != c.wantPhone {
				t.Errorf("normalizePhone(%q) = (%q, %q), want (%q, %q)",
					c.in, zone, phone, c.wantZone, c.wantPhone)
			}
			// 不变量 2:同进同退。
			if (zone == "") != (phone == "") {
				t.Errorf("normalizePhone(%q) returned a half-filled pair (%q, %q); "+
					"callers treat a non-empty phone as 'zone is usable too'", c.in, zone, phone)
			}
		})
	}
}

// extractZone / extractPhone 是既有调用点的入口(9 处),必须与 normalizePhone
// 给出一致的结果 —— 否则 UIDsByPhone 会拿到一对不匹配的参数。
func TestExtractZonePhone_AgreeWithNormalize(t *testing.T) {
	for _, in := range []string{
		"+8613812345678", "13812345678", "008613812345678", "8613812345678",
		"", "11136618971", "+46701234567", "not-a-phone",
	} {
		wantZone, wantPhone := normalizePhone(in)
		if got := extractZone(in); got != wantZone {
			t.Errorf("extractZone(%q) = %q, want %q", in, got, wantZone)
		}
		if got := extractPhone(in); got != wantPhone {
			t.Errorf("extractPhone(%q) = %q, want %q", in, got, wantPhone)
		}
	}
}
