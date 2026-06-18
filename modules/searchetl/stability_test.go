package searchetl

import "testing"

func row(id, createdUnix int64) *srcMessageRow {
	return &srcMessageRow{ID: id, CreatedUnix: createdUnix}
}

// TestStablePrefix_AllStable 全部行落库已超过 lag → 整批稳定。
func TestStablePrefix_AllStable(t *testing.T) {
	rows := []*srcMessageRow{row(1, 100), row(2, 110), row(3, 120)}
	cutoff := int64(200) // now-lag，远大于所有 created
	got := stablePrefix(rows, cutoff)
	if len(got) != 3 {
		t.Fatalf("want 3 stable, got %d", len(got))
	}
}

// TestStablePrefix_TailUnstable 批尾若干行未满 lag → 只取稳定前缀，游标不越过未稳定行。
func TestStablePrefix_TailUnstable(t *testing.T) {
	rows := []*srcMessageRow{row(1, 100), row(2, 110), row(3, 250), row(4, 260)}
	cutoff := int64(200)
	got := stablePrefix(rows, cutoff)
	if len(got) != 2 {
		t.Fatalf("want 2 stable, got %d", len(got))
	}
	if got[len(got)-1].ID != 2 {
		t.Fatalf("stable prefix must end at id=2, got id=%d", got[len(got)-1].ID)
	}
}

// TestStablePrefix_HeadUnstable 队首即未稳定 → 空前缀，本轮不前进。
func TestStablePrefix_HeadUnstable(t *testing.T) {
	rows := []*srcMessageRow{row(1, 300), row(2, 310)}
	cutoff := int64(200)
	got := stablePrefix(rows, cutoff)
	if len(got) != 0 {
		t.Fatalf("want 0 stable (head unstable), got %d", len(got))
	}
}

// TestStablePrefix_LowIdLateCommit C1 核心反例：低 id 行晚提交（created 更晚）混在高稳定行后。
// 因游标按稳定前缀末尾推进，未稳定的低 id 行（id=3, created=250）会被保留，下轮重读，
// 不会被越过漏扫。验证「首个未稳定行之后一律视为未稳定」的无空洞前缀语义。
func TestStablePrefix_LowIdLateCommit(t *testing.T) {
	// id 升序，但 id=3 落库晚（晚提交）→ created=250 未满 lag。
	rows := []*srcMessageRow{row(1, 100), row(2, 110), row(3, 250), row(4, 130)}
	cutoff := int64(200)
	got := stablePrefix(rows, cutoff)
	// 截断在首个未稳定行（id=3）处，即便 id=4 自身 created 已稳定也不纳入（无空洞保证）。
	if len(got) != 2 || got[len(got)-1].ID != 2 {
		t.Fatalf("want prefix ending at id=2 (no-hole cut at first unstable), got len=%d", len(got))
	}
}

// TestStablePrefix_Empty 空批 → 空前缀。
func TestStablePrefix_Empty(t *testing.T) {
	if got := stablePrefix(nil, 100); len(got) != 0 {
		t.Fatalf("want 0 on empty, got %d", len(got))
	}
}

// TestFirstNonAscendingByID_StrictAscending 严格升序 → 无违规（返回 -1）。
func TestFirstNonAscendingByID_StrictAscending(t *testing.T) {
	rows := []*srcMessageRow{row(1, 0), row(2, 0), row(5, 0), row(9, 0)}
	if got := firstNonAscendingByID(rows); got != -1 {
		t.Fatalf("strictly ascending must return -1, got %d", got)
	}
}

// TestFirstNonAscendingByID_DetectsViolation 期序断言核心：返回序破坏（回退或重复 id）→
// 报告首个违规下标。防索引/DB 版本变更悄悄破坏 ORDER BY id ASC 导致 stablePrefix 错位漏读。
func TestFirstNonAscendingByID_DetectsViolation(t *testing.T) {
	// id 在下标 2 处回退（5 -> 3）。
	if got := firstNonAscendingByID([]*srcMessageRow{row(1, 0), row(5, 0), row(3, 0)}); got != 2 {
		t.Fatalf("descending at index 2 must be reported, got %d", got)
	}
	// 重复 id（非严格升序）也算违规。
	if got := firstNonAscendingByID([]*srcMessageRow{row(1, 0), row(2, 0), row(2, 0)}); got != 2 {
		t.Fatalf("duplicate id at index 2 must be reported, got %d", got)
	}
}

// TestFirstNonAscendingByID_ShortInputs 空/单行无违规。
func TestFirstNonAscendingByID_ShortInputs(t *testing.T) {
	if got := firstNonAscendingByID(nil); got != -1 {
		t.Fatalf("nil must return -1, got %d", got)
	}
	if got := firstNonAscendingByID([]*srcMessageRow{row(7, 0)}); got != -1 {
		t.Fatalf("single row must return -1, got %d", got)
	}
}
