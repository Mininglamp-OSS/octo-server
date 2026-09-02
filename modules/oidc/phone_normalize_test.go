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

		// 带国家码的三种写法
		{"zone prefix 0086", "008613812345678", "0086", "13812345678"},
		{"country code without plus", "8613812345678", "0086", "13812345678"},

		// 裸号**不推断**。见下方 TestNormalizePhone_BareNumberIsNotInferred:
		// 北美号码与中国号段在裸号形态下无法区分。
		{"bare 11 digits is not inferred", "13812345678", "", ""},

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

// 裸号(不带国家码)一律不推断归属地。
//
// 上一版把匹配 `^1[3-9]\d{9}$` 的裸号推定为中国大陆号,依据是厂商手册 userinfo
// 里的一个示例。那个推断是错的,而且错得很具体:
//
// 北美编号计划(NANP)的号码是 `1` + 三位区号,而区号首位取 [2-9]。于是
// **约 7/8 的北美号码**在裸号形态下与中国号段完全同形:
//
//	+1 386-123-4567  →  13861234567  ← 138 是真实的中国移动号段
//	+1 415-555-2671  →  14155552671  ← 141 同理
//
// 存进去就是**某个中国人的真实号码**,而且那一行看起来完全正常 —— 这正是
// normalizePhone 注释里写明要避免的失效模式("猜错国家码的后果是把号码存成
// 另一个国家的另一个号")。brief 也记录了这家客户在多个国家有员工。
//
// 另一个方向的证据:手册那个示例 `11136618971` 以 `111` 开头,根本不是合法的
// 中国号段。也就是说我们**从未**见过上游发出合法的裸中国号,那个推断从头到尾
// 建立在一个假造的示例上。
//
// 所以恢复成"必须带国家码"。真确认上游发裸号之后,按实测到的形态再加回来。
func TestNormalizePhone_BareNumberIsNotInferred(t *testing.T) {
	// 这些字符串既是合法的中国手机号,也是合法的 NANP 号码。
	ambiguous := []string{
		"13861234567", // +1 386-123-4567 / CN 138 号段
		"14155552671", // +1 415-555-2671 / CN 141 号段
		"13105551234", // +1 310-555-1234 / CN 131 号段
		"19175551234", // +1 917-555-1234 / CN 191 号段
	}
	for _, in := range ambiguous {
		zone, phone := normalizePhone(in)
		if zone != "" || phone != "" {
			t.Errorf("normalizePhone(%q) = (%q, %q), want an empty pair: this string is "+
				"simultaneously a valid CN mobile and a valid NANP number, so inferring "+
				"0086 stores a real Chinese number belonging to someone else",
				in, zone, phone)
		}
	}
}

// 反面:带上国家码之后必须能解出来 —— 否则这就不是"不猜",而是"不支持"。
func TestNormalizePhone_ExplicitCountryCodeStillWorks(t *testing.T) {
	for _, in := range []string{
		"+8613861234567", "008613861234567", "8613861234567",
		"+86 138 6123 4567", "+86-138-6123-4567",
	} {
		zone, phone := normalizePhone(in)
		if zone != "0086" || phone != "13861234567" {
			t.Errorf("normalizePhone(%q) = (%q, %q), want (0086, 13861234567)", in, zone, phone)
		}
	}
}
