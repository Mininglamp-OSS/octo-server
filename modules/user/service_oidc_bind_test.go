package user

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOIDCBindHandler 单元测试用,验证 *Service 是否正确地把 IService 的
// 三个 OIDC 绑定方法委托给注入的 handler。生产路径下由 *User 实现(集成测试覆盖)。
type fakeOIDCBindHandler struct {
	verifyPwdResp struct {
		matched bool
		reason  string
		err     error
	}
	verifyPwdReq struct {
		uid, password string
		called        bool
	}

	sendSMSErr error
	sendSMSReq struct {
		zone, phone string
		called      bool
	}

	verifySMSErr error
	verifySMSReq struct {
		zone, phone, code string
		called            bool
	}

	isBindableResp struct {
		ok  bool
		err error
	}
	isBindableReq struct {
		uid    string
		called bool
	}
}

func (f *fakeOIDCBindHandler) VerifyPasswordByUID(_ context.Context, uid, password string) (bool, string, error) {
	f.verifyPwdReq.uid = uid
	f.verifyPwdReq.password = password
	f.verifyPwdReq.called = true
	return f.verifyPwdResp.matched, f.verifyPwdResp.reason, f.verifyPwdResp.err
}

func (f *fakeOIDCBindHandler) SendOIDCBindSMS(_ context.Context, zone, phone string) error {
	f.sendSMSReq.zone = zone
	f.sendSMSReq.phone = phone
	f.sendSMSReq.called = true
	return f.sendSMSErr
}

func (f *fakeOIDCBindHandler) VerifyOIDCBindSMS(_ context.Context, zone, phone, code string) error {
	f.verifySMSReq.zone = zone
	f.verifySMSReq.phone = phone
	f.verifySMSReq.code = code
	f.verifySMSReq.called = true
	return f.verifySMSErr
}

func (f *fakeOIDCBindHandler) IsBindable(_ context.Context, uid string) (bool, error) {
	f.isBindableReq.uid = uid
	f.isBindableReq.called = true
	return f.isBindableResp.ok, f.isBindableResp.err
}

// TestService_VerifyPasswordByUID_DelegationAndErrors 锁定 *Service 委托
// 行为:未注入 handler 返 ErrOIDCBindNotConfigured;已注入则原样透传
// matched/reason/err 三个返回值,不做语义改写。
func TestService_VerifyPasswordByUID_DelegationAndErrors(t *testing.T) {
	t.Run("not configured returns sentinel", func(t *testing.T) {
		svc := &Service{}
		matched, reason, err := svc.VerifyPasswordByUID(context.Background(), "u1", "pwd")
		assert.False(t, matched)
		assert.Empty(t, reason)
		assert.ErrorIs(t, err, ErrOIDCBindNotConfigured)
	})

	t.Run("matched path", func(t *testing.T) {
		fake := &fakeOIDCBindHandler{}
		fake.verifyPwdResp.matched = true
		svc := &Service{bindHandler: fake}

		matched, reason, err := svc.VerifyPasswordByUID(context.Background(), "u1", "pwd")
		require.NoError(t, err)
		assert.True(t, matched)
		assert.Empty(t, reason)
		assert.True(t, fake.verifyPwdReq.called)
		assert.Equal(t, "u1", fake.verifyPwdReq.uid)
		assert.Equal(t, "pwd", fake.verifyPwdReq.password)
	})

	t.Run("mismatch reason propagated", func(t *testing.T) {
		fake := &fakeOIDCBindHandler{}
		fake.verifyPwdResp.matched = false
		fake.verifyPwdResp.reason = BindReasonPasswordMismatch
		svc := &Service{bindHandler: fake}

		matched, reason, err := svc.VerifyPasswordByUID(context.Background(), "u1", "pwd")
		require.NoError(t, err)
		assert.False(t, matched)
		assert.Equal(t, BindReasonPasswordMismatch, reason)
	})

	t.Run("infrastructure error wrapped through", func(t *testing.T) {
		dbErr := errors.New("db timeout")
		fake := &fakeOIDCBindHandler{}
		fake.verifyPwdResp.err = dbErr
		svc := &Service{bindHandler: fake}

		_, _, err := svc.VerifyPasswordByUID(context.Background(), "u1", "pwd")
		assert.ErrorIs(t, err, dbErr)
	})
}

// TestService_SendOIDCBindSMS_DelegationAndErrors 与 VerifyPassword 同款委托模式。
func TestService_SendOIDCBindSMS_DelegationAndErrors(t *testing.T) {
	t.Run("not configured returns sentinel", func(t *testing.T) {
		svc := &Service{}
		err := svc.SendOIDCBindSMS(context.Background(), "0086", "13900000000")
		assert.ErrorIs(t, err, ErrOIDCBindNotConfigured)
	})

	t.Run("forwards zone/phone verbatim", func(t *testing.T) {
		fake := &fakeOIDCBindHandler{}
		svc := &Service{bindHandler: fake}

		require.NoError(t, svc.SendOIDCBindSMS(context.Background(), "0086", "13900000000"))
		assert.True(t, fake.sendSMSReq.called)
		assert.Equal(t, "0086", fake.sendSMSReq.zone)
		assert.Equal(t, "13900000000", fake.sendSMSReq.phone)
	})

	t.Run("provider error bubbles", func(t *testing.T) {
		fake := &fakeOIDCBindHandler{sendSMSErr: errors.New("sms provider down")}
		svc := &Service{bindHandler: fake}
		err := svc.SendOIDCBindSMS(context.Background(), "0086", "13900000000")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "sms provider down")
	})
}

// TestService_VerifyOIDCBindSMS_DelegationAndErrors 与 SendOIDCBindSMS 对称。
func TestService_VerifyOIDCBindSMS_DelegationAndErrors(t *testing.T) {
	t.Run("not configured returns sentinel", func(t *testing.T) {
		svc := &Service{}
		err := svc.VerifyOIDCBindSMS(context.Background(), "0086", "13900000000", "1234")
		assert.ErrorIs(t, err, ErrOIDCBindNotConfigured)
	})

	t.Run("forwards code verbatim", func(t *testing.T) {
		fake := &fakeOIDCBindHandler{}
		svc := &Service{bindHandler: fake}

		require.NoError(t, svc.VerifyOIDCBindSMS(context.Background(), "0086", "13900000000", "9876"))
		assert.True(t, fake.verifySMSReq.called)
		assert.Equal(t, "9876", fake.verifySMSReq.code)
	})

	t.Run("provider error bubbles", func(t *testing.T) {
		fake := &fakeOIDCBindHandler{verifySMSErr: errors.New("code expired")}
		svc := &Service{bindHandler: fake}
		err := svc.VerifyOIDCBindSMS(context.Background(), "0086", "13900000000", "1234")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "code expired")
	})
}

// TestService_IsBindable_DelegationAndErrors 同 VerifyPasswordByUID 模式:
// 未注入 sentinel,已注入直接透传 ok/err。Confirm-time TOCTOU 复核的新方法。
func TestService_IsBindable_DelegationAndErrors(t *testing.T) {
	t.Run("not configured returns sentinel", func(t *testing.T) {
		svc := &Service{}
		ok, err := svc.IsBindable(context.Background(), "u1")
		assert.False(t, ok)
		assert.ErrorIs(t, err, ErrOIDCBindNotConfigured)
	})

	t.Run("forwards uid verbatim, returns ok=true", func(t *testing.T) {
		fake := &fakeOIDCBindHandler{}
		fake.isBindableResp.ok = true
		svc := &Service{bindHandler: fake}

		ok, err := svc.IsBindable(context.Background(), "u-alice")
		assert.NoError(t, err)
		assert.True(t, ok)
		assert.True(t, fake.isBindableReq.called)
		assert.Equal(t, "u-alice", fake.isBindableReq.uid)
	})

	t.Run("returns ok=false without error for unbindable account", func(t *testing.T) {
		fake := &fakeOIDCBindHandler{}
		// 默认 isBindableResp.ok=false
		svc := &Service{bindHandler: fake}

		ok, err := svc.IsBindable(context.Background(), "u-disabled")
		assert.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("infrastructure error bubbles", func(t *testing.T) {
		fake := &fakeOIDCBindHandler{}
		fake.isBindableResp.err = errors.New("db timeout")
		svc := &Service{bindHandler: fake}

		ok, err := svc.IsBindable(context.Background(), "u-x")
		assert.False(t, ok)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "db timeout")
	})
}
