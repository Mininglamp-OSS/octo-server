package oidc

// bind_snapshot_bounds_test.go — 升级前签发的绑定快照必须在**消费时**重新过守卫。
//
// 新加的可落库守卫跑在快照**签发**处(BindService.IssueWithReason)。但升级那一刻
// 已经躺在 Redis 里的快照,是上一个版本签发的 —— 而上一个版本在标准 OIDC 那条路
// **没有**任何长度上限(那正是上一轮的 blocker)。
//
// 这个代码库是**刻意**消费升级前快照的:bind_claims_compat_test.go 就钉着
// "旧格式快照必须仍能解出来",理由是升级瞬间在途的绑定流程不能全挂。于是两条
// 事实合起来构成一个缺口:在 bind token TTL 窗口内(默认 5 分钟,运维可调大),
// 一份带超长 sub 的旧快照仍能走完 Confirm / Create,重现这个 PR 花了三轮堵的
// 那两种结局 —— 严格 sql_mode 下 Create 留下没有 identity 行的孤立用户;
// 非严格模式下值被截断,前 255 字节相同的两个 subject 合成同一行(不可逆)。
//
// 这是矩阵里没被枚举的**时间**维度:P5 的 G3/G4 是按"签发时有守卫"打的 ✅,
// 而快照的寿命跨越了部署边界。
//
// 复核放在 decodeClaimsSnapshot 之后、任何写操作之前,与本文件既有的
// issuerAllowedForCreate 防御性复核同一个位置和同一个理由(TTL 窗口内世界会变)。

import (
	"context"
	"strings"
	"testing"
	"time"
)

// legacyOversizedSnapshot 造一份**升级前**形态的 claims 快照。
//
// 手写 JSON 而不是序列化当前结构体:后者会被当前代码的约束塑形,测不出
// "上一个版本能写出什么"。这与 bind_claims_compat_test.go 是同一个手法。
func legacyOversizedSnapshot(sub string) []byte {
	return []byte(`{
	  "iss": "https://idp.example",
	  "sub": "` + sub + `",
	  "email": "legacy@example.com",
	  "email_verified": true,
	  "phone_number": "+8613000000000",
	  "phone_number_verified": true,
	  "name": "Legacy User"
	}`)
}

func TestBindConfirm_RefusesPreUpgradeSnapshotWithUnstorableSubject(t *testing.T) {
	h := newBindHarness(t)
	oversized := strings.Repeat("s", subjectMaxLen+1)

	sess := &BindSession{
		JTI:            "jti-legacy-confirm",
		Issuer:         "https://idp.example",
		Subject:        oversized,
		CandidateUID:   "u-existing",
		ClaimsSnapshot: legacyOversizedSnapshot(oversized),
		SDSnapshot:     []byte(`{"ip":"203.0.113.7","device_flag":0}`),
		Status:         BindStatusVerified,
		VerifiedMethod: BindMethodPassword,
		CreatedAt:      time.Now().Unix(),
	}
	if err := h.store.Save(context.Background(), sess, time.Minute); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	resp, err := h.svc.Confirm(context.Background(), sess.JTI)
	if err == nil {
		t.Fatalf("Confirm accepted a pre-upgrade snapshot carrying a %d-byte subject "+
			"(column width %d); resp=%+v", len(oversized), subjectMaxLen, resp)
	}
	if n := len(h.identity.inserted); n != 0 {
		t.Errorf("an identity row was written for an unstorable subject: %+v", h.identity.inserted)
	}
	if n := h.users.callCnt; n != 0 {
		t.Errorf("IssueSession ran for an unstorable subject: %d call(s)", n)
	}
}

func TestBindCreate_RefusesPreUpgradeSnapshotWithUnstorableSubject(t *testing.T) {
	h := newBindHarness(t)
	oversized := strings.Repeat("s", subjectMaxLen+1)

	sess := &BindSession{
		JTI:            "jti-legacy-create",
		Issuer:         "https://idp.example",
		Subject:        oversized,
		ClaimsSnapshot: legacyOversizedSnapshot(oversized),
		SDSnapshot:     []byte(`{"ip":"203.0.113.7","device_flag":0}`),
		Status:         BindStatusIssued,
		IssueReason:    BindReasonUnknownUser,
		CreatedAt:      time.Now().Unix(),
	}
	if err := h.store.Save(context.Background(), sess, time.Minute); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	resp, err := h.svc.Create(context.Background(), sess.JTI)
	if err == nil {
		t.Fatalf("Create accepted a pre-upgrade snapshot carrying a %d-byte subject; resp=%+v",
			len(oversized), resp)
	}
	// Create 的顺序是先建用户再插 identity —— 所以"用户没被创建"才是真正要守的那条。
	if n := h.users.callCnt; n != 0 {
		t.Errorf("IssueSession created a user before the unstorable subject was refused; "+
			"under strict sql_mode that leaves an orphan with no identity row: %d call(s)", n)
	}
	if n := len(h.identity.inserted); n != 0 {
		t.Errorf("an identity row was written: %+v", h.identity.inserted)
	}
}

// 工号形态的下限对旧快照同样是新的:它这一轮才加到标准 OIDC 那条路上,
// 而旧快照正是那条路签发的。
func TestBindCreate_RefusesPreUpgradeSnapshotWithShortNumericSubject(t *testing.T) {
	h := newBindHarness(t)

	sess := &BindSession{
		JTI:            "jti-legacy-empno",
		Issuer:         "https://idp.example",
		Subject:        "7654321",
		ClaimsSnapshot: legacyOversizedSnapshot("7654321"),
		SDSnapshot:     []byte(`{"ip":"203.0.113.7","device_flag":0}`),
		Status:         BindStatusIssued,
		IssueReason:    BindReasonUnknownUser,
		CreatedAt:      time.Now().Unix(),
	}
	if err := h.store.Save(context.Background(), sess, time.Minute); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	if _, err := h.svc.Create(context.Background(), sess.JTI); err == nil {
		t.Fatal("Create accepted a pre-upgrade snapshot with a 7-digit numeric subject; " +
			"employee numbers are reused, and this snapshot came from the standard-OIDC " +
			"path, which only gained the shape guard in this change")
	}
	if h.users.callCnt != 0 || len(h.identity.inserted) != 0 {
		t.Error("side effects ran for a refused subject")
	}
}

// 反面:形态正常的旧快照必须照常走完 —— 否则这道复核就把升级瞬间在途的
// 绑定流程全弄挂了,而那正是 bind_claims_compat_test.go 存在的理由。
func TestBindCreate_AcceptsWellFormedPreUpgradeSnapshot(t *testing.T) {
	h := newBindHarness(t)
	const sub = "823071756087671783"

	sess := &BindSession{
		JTI:            "jti-legacy-ok",
		Issuer:         "https://idp.example",
		Subject:        sub,
		ClaimsSnapshot: legacyOversizedSnapshot(sub),
		SDSnapshot:     []byte(`{"ip":"203.0.113.7","device_flag":0}`),
		Status:         BindStatusIssued,
		IssueReason:    BindReasonUnknownUser,
		CreatedAt:      time.Now().Unix(),
	}
	if err := h.store.Save(context.Background(), sess, time.Minute); err != nil {
		t.Fatalf("seed session: %v", err)
	}

	if _, err := h.svc.Create(context.Background(), sess.JTI); err != nil {
		t.Fatalf("a well-formed pre-upgrade snapshot was refused; in-flight bind sessions "+
			"would break on deploy: %v", err)
	}
}
