package user

import (
	"os"
	"testing"
)

func withPhoneSecretForTest(t *testing.T, secret string) {
	t.Helper()
	old, hadOld := os.LookupEnv(phoneEncryptionSecretEnv)
	if secret == "" {
		os.Unsetenv(phoneEncryptionSecretEnv)
	} else {
		os.Setenv(phoneEncryptionSecretEnv, secret)
	}
	t.Cleanup(func() {
		if hadOld {
			os.Setenv(phoneEncryptionSecretEnv, old)
		} else {
			os.Unsetenv(phoneEncryptionSecretEnv)
		}
	})
}

func TestNewPhoneEncryptor_MissingKey(t *testing.T) {
	withPhoneSecretForTest(t, "")
	if _, err := newPhoneEncryptor(); err == nil {
		t.Fatal("expected error when OCTO_PII_ENCRYPTION_SECRET is unset")
	}
	if _, err := PhoneBlindHash("0086", "13800000000"); err == nil {
		t.Fatal("expected error when OCTO_PII_ENCRYPTION_SECRET is unset")
	}
}

func TestNewPhoneEncryptor_WrongKeyLength(t *testing.T) {
	withPhoneSecretForTest(t, "too-short")
	if _, err := newPhoneEncryptor(); err == nil {
		t.Fatal("expected error for a non-32-byte key")
	}
}

func TestEncryptDecryptPhone_RoundTrip(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	enc, err := newPhoneEncryptor()
	if err != nil {
		t.Fatalf("newPhoneEncryptor: %v", err)
	}
	encrypted, hash, last4, err := enc.encryptPhone("0086", "13800001234")
	if err != nil {
		t.Fatalf("encryptPhone: %v", err)
	}
	if last4 != "1234" {
		t.Fatalf("last4 = %q, want 1234", last4)
	}
	if hash == "" {
		t.Fatal("hash must not be empty")
	}
	plaintext, err := enc.decryptPhone(encrypted)
	if err != nil {
		t.Fatalf("decryptPhone: %v", err)
	}
	if want := phoneCryptoInput("0086", "13800001234"); plaintext != want {
		t.Fatalf("decryptPhone = %q, want %q", plaintext, want)
	}
}

func TestEncryptPhone_NonceRandomized(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	enc, err := newPhoneEncryptor()
	if err != nil {
		t.Fatalf("newPhoneEncryptor: %v", err)
	}
	a, _, _, err := enc.encryptPhone("0086", "13800001234")
	if err != nil {
		t.Fatalf("encryptPhone: %v", err)
	}
	b, _, _, err := enc.encryptPhone("0086", "13800001234")
	if err != nil {
		t.Fatalf("encryptPhone: %v", err)
	}
	if string(a) == string(b) {
		t.Fatal("two encryptions of the same phone must not produce identical ciphertext (random nonce)")
	}
}

func TestPhoneBlindHash_DeterministicAndZoneScoped(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	h1, err := PhoneBlindHash("0086", "13800001234")
	if err != nil {
		t.Fatalf("PhoneBlindHash: %v", err)
	}
	h2, err := PhoneBlindHash("0086", "13800001234")
	if err != nil {
		t.Fatalf("PhoneBlindHash: %v", err)
	}
	if h1 != h2 {
		t.Fatal("PhoneBlindHash must be deterministic for the same zone+phone")
	}
	h3, err := PhoneBlindHash("0001", "13800001234")
	if err != nil {
		t.Fatalf("PhoneBlindHash: %v", err)
	}
	if h1 == h3 {
		t.Fatal("PhoneBlindHash must differ across zones for the same phone digits")
	}
}

// TestSyncPhoneShadow_DegradesAndPopulates 覆盖 DB 层影子列同步的三种情形。
// 同步点在 DB.Insert/insertTx 里，所以任何建号路径都会自动带上影子列 —— 这条测试
// 保护的是"降级不阻断"与"有手机号必填齐"两个性质。
func TestSyncPhoneShadow_DegradesAndPopulates(t *testing.T) {
	// 主密钥缺失：清零影子列，明文 Phone 保留（降级而非阻断）
	d := &DB{}
	m := &Model{Zone: "0086", Phone: "13800001234"}
	d.syncPhoneShadow(m)
	if m.PhoneHash != "" || len(m.PhoneEncrypted) != 0 || m.PhoneLast4 != "" {
		t.Fatal("主密钥缺失时影子列必须为空")
	}
	if m.Phone != "13800001234" {
		t.Fatal("降级不得动明文 phone 列")
	}

	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	enc, err := newPhoneEncryptor()
	if err != nil {
		t.Fatalf("newPhoneEncryptor: %v", err)
	}
	d.phoneEnc = enc

	// 无手机号：影子列为空
	empty := &Model{Zone: "0086"}
	d.syncPhoneShadow(empty)
	if empty.PhoneHash != "" || len(empty.PhoneEncrypted) != 0 {
		t.Fatal("无手机号时影子列必须为空")
	}

	// 有手机号：三列齐备
	filled := &Model{Zone: "0086", Phone: "13800001234"}
	d.syncPhoneShadow(filled)
	if filled.PhoneHash == "" || len(filled.PhoneEncrypted) == 0 || filled.PhoneLast4 != "1234" {
		t.Fatal("有手机号且密钥就绪时三个影子列都应填齐")
	}

	// 复用同一 Model 但清掉手机号：影子列必须被清零，不能留下陈旧值
	// （陈旧影子列比明文列活得更久，就是注销后手机号无法复用的那类 P0 根因）
	filled.Phone = ""
	d.syncPhoneShadow(filled)
	if filled.PhoneHash != "" || len(filled.PhoneEncrypted) != 0 || filled.PhoneLast4 != "" {
		t.Fatal("手机号被清空时影子列必须一起清零，不得残留陈旧盲索引")
	}
}
