package group

// scanJoinIdentity 扫码入群时扫码者在群内的身份。
type scanJoinIdentity struct {
	IsExternal    int    // 1 = 以外部成员身份入群
	SourceSpaceID string // 外部成员的来源 Space（is_external=0 时为空）
	Forbidden     bool   // 群禁止外部成员，且扫码者不在群所属 Space
	// HeaderIgnored 客户端传了 X-Space-ID 但不可用（非成员 / 校验失败），已忽略。
	HeaderIgnored bool
	HeaderErr     error
}

// resolveScanJoinIdentity 决定扫码者以哪个 Space 的身份入群。
//
// 原则：用户以「当前所在 Space」（X-Space-ID）的身份入群。
//   - 扫码者不在群所属 Space：外部成员，source_space_id 取当前 Space，
//     当前 Space 不可用时兜底 home Space（YUJ-199 / YUJ-201 既有语义）。
//   - 扫码者同时在群所属 Space 与当前 Space（当前 Space ≠ 群 Space）且群允许
//     外部成员：以当前 Space 身份作为外部成员入群。否则同时属于 A/B 两个 Space
//     的用户在 A 视角扫 B 的群，会被强制以 B 成员身份入群、群只出现在 B，
//     客户端随之跳到 B，A 身份无法加入 B 的群。
//   - 群禁止外部成员时，群 Space 成员仍按内部成员入群（不因身份选择失去原有权限）。
//
// isMember 只会以群 Space 和 X-Space-ID 调用，最多两次主键/唯一索引查询。
func resolveScanJoinIdentity(
	groupSpaceID string,
	allowExternal int,
	headerSpaceID string,
	isMember func(spaceID string) (bool, error),
	defaultSpaceID func() string,
) (scanJoinIdentity, error) {
	var id scanJoinIdentity
	if groupSpaceID == "" {
		return id, nil
	}
	inGroupSpace, err := isMember(groupSpaceID)
	if err != nil {
		return id, err
	}

	headerValid := false
	if headerSpaceID != "" && headerSpaceID != groupSpaceID {
		ok, headerErr := isMember(headerSpaceID)
		if headerErr != nil || !ok {
			id.HeaderIgnored = true
			id.HeaderErr = headerErr
		} else {
			headerValid = true
		}
	}

	if inGroupSpace {
		if headerValid && allowExternal != 0 {
			id.IsExternal = 1
			id.SourceSpaceID = headerSpaceID
		}
		return id, nil
	}
	if allowExternal == 0 {
		id.Forbidden = true
		return id, nil
	}
	id.IsExternal = 1
	if headerValid {
		id.SourceSpaceID = headerSpaceID
	} else {
		id.SourceSpaceID = defaultSpaceID()
	}
	return id, nil
}
