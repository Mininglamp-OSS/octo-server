package user

import (
	"errors"
	"os"
	"strings"
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

func TestPhoneBlindHashUsesNewEncodingVersionWithoutRotatingLoginAuditHash(t *testing.T) {
	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	phoneHash, err := PhoneBlindHash("0086", "13800001234")
	if err != nil {
		t.Fatalf("PhoneBlindHash: %v", err)
	}
	if !strings.HasPrefix(phoneHash, "2:") {
		t.Fatalf("PhoneBlindHash = %q, want encoding version 2", phoneHash)
	}

	loginHash, err := LoginAccountBlindHash("13800001234")
	if err != nil {
		t.Fatalf("LoginAccountBlindHash: %v", err)
	}
	if !strings.HasPrefix(loginHash, "1:") {
		t.Fatalf("LoginAccountBlindHash = %q, phone encoding change must not rotate audit hashes", loginHash)
	}
}

func TestPhoneCryptoInputIsUnambiguous(t *testing.T) {
	a := phoneCryptoInput("0086|1", "3800001234")
	b := phoneCryptoInput("0086", "1|3800001234")
	if a == b {
		t.Fatalf("different zone/phone tuples produced the same crypto input %q", a)
	}
}

// TestSyncPhoneShadow_FailsClosedAndPopulates 覆盖 DB 层影子列同步的契约。
// 同步点在 DB.Insert/insertTx 里，所以任何建号路径都会自动带上影子列。
func TestSyncPhoneShadow_FailsClosedAndPopulates(t *testing.T) {
	// 主密钥缺失 + 有手机号：fail-closed，不得降级成"只写明文"
	d := &DB{}
	m := &Model{Zone: "0086", Phone: "13800001234"}
	err := d.syncPhoneShadow(m)
	if !errors.Is(err, ErrPhoneEncryptionUnavailable) {
		t.Fatalf("缺密钥且有手机号时必须返回 ErrPhoneEncryptionUnavailable，实际=%v", err)
	}

	// 主密钥缺失 + 无手机号：放行（否则缺密钥会阻断 username/email/OAuth/机器人建号）
	noPhone := &Model{Zone: "0086"}
	if err := d.syncPhoneShadow(noPhone); err != nil {
		t.Fatalf("缺密钥但无手机号时应放行，实际=%v", err)
	}
	if noPhone.PhoneHash != "" || len(noPhone.PhoneEncrypted) != 0 {
		t.Fatal("无手机号时影子列必须为空")
	}

	withPhoneSecretForTest(t, "0123456789abcdef0123456789abcdef")
	enc, err := newPhoneEncryptor()
	if err != nil {
		t.Fatalf("newPhoneEncryptor: %v", err)
	}
	d.phoneEnc = enc

	// 有手机号：三列齐备
	filled := &Model{Zone: "0086", Phone: "13800001234"}
	if err := d.syncPhoneShadow(filled); err != nil {
		t.Fatalf("syncPhoneShadow: %v", err)
	}
	if filled.PhoneHash == "" || len(filled.PhoneEncrypted) == 0 || filled.PhoneLast4 != "1234" {
		t.Fatal("有手机号且密钥就绪时三个影子列都应填齐")
	}

	// 复用同一 Model 但清掉手机号：影子列必须被清零，不能留下陈旧值
	// （陈旧影子列比明文列活得更久，就是注销后手机号无法复用的那类 P0 根因）
	filled.Phone = ""
	if err := d.syncPhoneShadow(filled); err != nil {
		t.Fatalf("syncPhoneShadow: %v", err)
	}
	if filled.PhoneHash != "" || len(filled.PhoneEncrypted) != 0 || filled.PhoneLast4 != "" {
		t.Fatal("手机号被清空时影子列必须一起清零，不得残留陈旧盲索引")
	}
}
