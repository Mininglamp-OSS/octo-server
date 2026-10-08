package oidc

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-lib/pkg/db"

	mysql "github.com/go-sql-driver/mysql"
)

// 本文件覆盖 issue #920:上游轮换 sub 后,email/phone 自动绑定路径必须
// UPDATE 既有 (uid, issuer) 行的 subject,而不是盲 INSERT 撞 uk_uid_issuer
// 把用户永久锁死。行为矩阵见 oidc-sub-rotation-design.md §4。

const (
	rotIssuer = "https://aegis.example"
	rotUID    = "u-rot-1"
)

// rotClaims 返回携带新 sub 的 claims(邮箱已验证,走 email 自动绑定)。
func rotClaims() *IDTokenClaims {
	return &IDTokenClaims{
		Issuer:        rotIssuer,
		Subject:       "sub-new",
		Email:         "alice@example.com",
		EmailVerified: true,
	}
}

// rotStore 预置一行旧 sub 绑定:step 1 按新 sub 查不到,只能靠 email 匹配到 uid。
func rotStore() *fakeIdentityStore {
	store := newFakeIdentityStore()
	_ = store.Insert(&IdentityModel{
		BaseModel: db.BaseModel{Id: 77},
		UID:       rotUID,
		Issuer:    rotIssuer,
		Subject:   "sub-old",
	})
	return store
}

// rotUserLookup 预置 email → uid 唯一命中。
func rotUserLookup() *fakeUserLookup {
	return &fakeUserLookup{
		usersByEmail: map[string][]string{"alice@example.com": {rotUID}},
	}
}

// 矩阵 1:email 命中同 uid,已有 (uid, issuer) 旧 sub → UPDATE subject,返回该 uid。
func TestService_ResolveOrLink_SubRotation_Email(t *testing.T) {
	store := rotStore()
	svc := newService(defaultProviderCfg(), store, rotUserLookup())

	res, err := svc.ResolveOrLink(context.Background(), rotClaims())
	if err != nil {
		t.Fatalf("ResolveOrLink: %v", err)
	}
	if res.UID != rotUID || res.IsNew {
		t.Fatalf("UID = %q IsNew = %v, want %q / false", res.UID, res.IsNew, rotUID)
	}
	if got := len(store.subjectUpdates); got != 1 {
		t.Fatalf("subjectUpdates = %d, want 1", got)
	}
	if id, subj := store.subjectUpdates[0].id, store.subjectUpdates[0].subject; id != 77 || subj != "sub-new" {
		t.Errorf("UpdateSubject(%d, %q), want (77, sub-new)", id, subj)
	}
	// 旧行必须消失,新行可被 Get 命中 —— 否则下一次登录又从 step 1 miss 开始。
	if old := store.bindings[rotIssuer+"|sub-old"]; old != nil {
		t.Error("old subject binding still present after rotation")
	}
	if cur := store.bindings[rotIssuer+"|sub-new"]; cur == nil || cur.UID != rotUID {
		t.Error("rotated binding not queryable by new subject")
	}
	if got := len(store.written); got != 1 { // 预置那行
		t.Errorf("written = %d, want 1 (no new row inserted)", got)
	}
}

// 矩阵 2:phone 命中同 uid(verified),已有旧 sub → 同样走轮换。
func TestService_ResolveOrLink_SubRotation_Phone(t *testing.T) {
	store := rotStore()
	users := &fakeUserLookup{
		usersByPhone: map[string][]string{"0086|13800001111": {rotUID}},
	}
	svc := newService(defaultProviderCfg(), store, users)

	claims := &IDTokenClaims{
		Issuer:        rotIssuer,
		Subject:       "sub-new",
		PhoneNumber:   "+8613800001111",
		PhoneVerified: true,
	}
	res, err := svc.ResolveOrLink(context.Background(), claims)
	if err != nil {
		t.Fatalf("ResolveOrLink: %v", err)
	}
	if res.UID != rotUID || res.IsNew {
		t.Fatalf("UID = %q IsNew = %v, want %q / false", res.UID, res.IsNew, rotUID)
	}
	if got := len(store.subjectUpdates); got != 1 {
		t.Fatalf("subjectUpdates = %d, want 1", got)
	}
	if subj := store.subjectUpdates[0].subject; subj != "sub-new" {
		t.Errorf("rotated to %q, want sub-new", subj)
	}
}

// 矩阵 3:已有行 sub 与 claims 完全相同 → 幂等返回 uid,零写入。
func TestService_ResolveOrLink_SubRotation_SameSubject_Idempotent(t *testing.T) {
	store := newFakeIdentityStore()
	_ = store.Insert(&IdentityModel{
		BaseModel: db.BaseModel{Id: 88},
		UID:       rotUID,
		Issuer:    rotIssuer,
		Subject:   "sub-same",
	})
	users := &fakeUserLookup{
		usersByEmail: map[string][]string{"alice@example.com": {rotUID}},
	}
	svc := newService(defaultProviderCfg(), store, users)

	claims := rotClaims()
	claims.Subject = "sub-same"
	res, err := svc.ResolveOrLink(context.Background(), claims)
	if err != nil {
		t.Fatalf("ResolveOrLink: %v", err)
	}
	if res.UID != rotUID {
		t.Errorf("UID = %q, want %q", res.UID, rotUID)
	}
	if got := len(store.subjectUpdates); got != 0 {
		t.Errorf("subjectUpdates = %d, want 0", got)
	}
	if got := len(store.written); got != 1 { // 预置那行
		t.Errorf("written = %d, want 1 (no re-insert)", got)
	}
}

// 矩阵 4:无 (uid, issuer) 行 → 首次绑定,行为与改动前一致(回归)。
func TestService_ResolveOrLink_SubRotation_NoExistingRow_FirstLink(t *testing.T) {
	store := newFakeIdentityStore()
	users := &fakeUserLookup{
		usersByEmail: map[string][]string{"alice@example.com": {rotUID}},
	}
	svc := newService(defaultProviderCfg(), store, users)

	res, err := svc.ResolveOrLink(context.Background(), rotClaims())
	if err != nil {
		t.Fatalf("ResolveOrLink: %v", err)
	}
	if res.UID != rotUID || res.IsNew {
		t.Fatalf("UID = %q IsNew = %v, want %q / false", res.UID, res.IsNew, rotUID)
	}
	if got := len(store.written); got != 1 || store.written[0].UID != rotUID {
		t.Fatalf("first-link insert missing: %+v", store.written)
	}
	if got := len(store.subjectUpdates); got != 0 {
		t.Errorf("subjectUpdates = %d, want 0 on first link", got)
	}
}

// 矩阵 5:Insert 撞 1062,重查到本 uid 的行且 sub 不同 → 竞态恢复为轮换。
func TestService_ResolveOrLink_SubRotation_InsertRace_RecoverToRotation(t *testing.T) {
	store := rotStore()
	store.uidIssuerRaceWinner = &IdentityModel{
		BaseModel: db.BaseModel{Id: 77},
		UID:       rotUID,
		Issuer:    rotIssuer,
		Subject:   "sub-old",
	}
	// 让 Insert 无条件返 1062:第一次 GetByUIDIssuer 查不到(赛手未 commit),
	// Insert 撞键,重查拿到赢家行(旧 sub) → 轮换成新 sub。
	store.failInsertWithDuplicate = true
	store.bindings = map[string]*IdentityModel{} // 移除预置行,逼 Get 也 miss
	users := &fakeUserLookup{
		usersByEmail: map[string][]string{"alice@example.com": {rotUID}},
	}
	svc := newService(defaultProviderCfg(), store, users)

	res, err := svc.ResolveOrLink(context.Background(), rotClaims())
	if err != nil {
		t.Fatalf("ResolveOrLink: %v", err)
	}
	if res.UID != rotUID {
		t.Fatalf("UID = %q, want %q", res.UID, rotUID)
	}
	if got := len(store.subjectUpdates); got != 1 {
		t.Fatalf("subjectUpdates = %d, want 1 (race recovered via rotation)", got)
	}
	if subj := store.subjectUpdates[0].subject; subj != "sub-new" {
		t.Errorf("rotated to %q, want sub-new", subj)
	}
}

// 矩阵 6:Insert 撞 1062 且重查无行(新 sub 被别的 uid 抢绑)→ ErrConflictNeedManual。
func TestService_ResolveOrLink_SubRotation_InsertRace_NoRow_ManualConflict(t *testing.T) {
	store := newFakeIdentityStore()
	store.failInsertWithDuplicate = true // Insert 1062,重查永远查不到
	users := &fakeUserLookup{
		usersByEmail: map[string][]string{"alice@example.com": {rotUID}},
	}
	svc := newService(defaultProviderCfg(), store, users)

	_, err := svc.ResolveOrLink(context.Background(), rotClaims())
	if !errors.Is(err, ErrConflictNeedManual) {
		t.Fatalf("err = %v, want ErrConflictNeedManual (no raw 1062)", err)
	}
	if got := len(store.subjectUpdates); got != 0 {
		t.Errorf("subjectUpdates = %d, want 0", got)
	}
}

// 矩阵 7:UpdateSubject 撞 1062(新 sub 在 step 1 之后被别的 uid 抢绑)→ 人工,不覆盖。
func TestService_ResolveOrLink_SubRotation_UpdateDuplicate_ManualConflict(t *testing.T) {
	store := rotStore()
	store.updateSubjectErr = &mysql.MySQLError{Number: 1062, Message: "Duplicate entry"}
	users := &fakeUserLookup{
		usersByEmail: map[string][]string{"alice@example.com": {rotUID}},
	}
	svc := newService(defaultProviderCfg(), store, users)

	_, err := svc.ResolveOrLink(context.Background(), rotClaims())
	if !errors.Is(err, ErrConflictNeedManual) {
		t.Fatalf("err = %v, want ErrConflictNeedManual", err)
	}
	// 旧行必须原样保留 —— 绝不覆盖别的 uid 的所有权。
	if cur := store.bindings[rotIssuer+"|sub-old"]; cur == nil || cur.UID != rotUID {
		t.Errorf("original binding must stay intact, got %+v", cur)
	}
}

// 矩阵 8:GetByUIDIssuer 命中的行 issuer 仅大小写不同 → 折叠碰撞,响亮拒绝,零写入。
func TestService_ResolveOrLink_SubRotation_FoldedIssuerCollision(t *testing.T) {
	store := newFakeIdentityStore()
	_ = store.Insert(&IdentityModel{
		BaseModel: db.BaseModel{Id: 99},
		UID:       rotUID,
		Issuer:    "https://AEGIS.example", // 与 claims.issuer 仅大小写不同
		Subject:   "sub-old",
	})
	users := &fakeUserLookup{
		usersByEmail: map[string][]string{"alice@example.com": {rotUID}},
	}
	svc := newService(defaultProviderCfg(), store, users)

	_, err := svc.ResolveOrLink(context.Background(), rotClaims())
	if !errors.Is(err, ErrConflictNeedManual) {
		t.Fatalf("err = %v, want ErrConflictNeedManual (folded collision)", err)
	}
	if got := len(store.subjectUpdates); got != 0 {
		t.Errorf("subjectUpdates = %d, want 0 (must not touch folded row)", got)
	}
	if got := len(store.written); got != 1 { // 预置那行
		t.Errorf("written = %d, want 1 (no insert on folded collision)", got)
	}
}

// 矩阵 9:email 命中多条 uid → ErrConflictNeedManual(原行为回归)。
func TestService_ResolveOrLink_SubRotation_MultiUserMatches_ManualConflict(t *testing.T) {
	store := newFakeIdentityStore()
	users := &fakeUserLookup{
		usersByEmail: map[string][]string{"alice@example.com": {"u-a", "u-b"}},
	}
	svc := newService(defaultProviderCfg(), store, users)

	_, err := svc.ResolveOrLink(context.Background(), rotClaims())
	if !errors.Is(err, ErrConflictNeedManual) {
		t.Fatalf("err = %v, want ErrConflictNeedManual", err)
	}
	if got := len(store.subjectUpdates); got != 0 {
		t.Errorf("subjectUpdates = %d, want 0", got)
	}
	if got := len(store.written); got != 0 {
		t.Errorf("written = %d, want 0", got)
	}
}
