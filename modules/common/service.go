package common

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"go.uber.org/zap"
)

var onceSerce sync.Once

// IService IService
type IService interface {
	GetAppConfig() (*AppConfigResp, error)
	// 获取短编号
	GetShortno() (string, error)
	SetShortnoUsed(shortno string, business string) error
}

// NewService NewService
func NewService(ctx *config.Context) IService {
	return newService(ctx)
}

type service struct {
	ctx         *config.Context
	appConfigDB *appConfigDB
	shortnoDB   *shortnoDB
	shortnoLock sync.RWMutex
}

func newService(ctx *config.Context) *service {
	// if ctx.GetConfig().ShortNo.NumOn {
	onceSerce.Do(func() {
		go runGenShortnoTask(ctx)
	})
	// }

	return &service{
		ctx:         ctx,
		appConfigDB: newAppConfigDB(ctx),
		shortnoDB:   newShortnoDB(ctx),
	}
}

// GetAppConfig GetAppConfig
func (s *service) GetAppConfig() (*AppConfigResp, error) {
	appConfigM, err := s.appConfigDB.query()
	if err != nil {
		return nil, err
	}
	// A freshly migrated database or a maintenance cleanup can temporarily have
	// no app_config row. Treat that as feature defaults instead of panicking in
	// asynchronous login follow-up work such as welcome-message delivery.
	if appConfigM == nil {
		return &AppConfigResp{}, nil
	}

	return &AppConfigResp{
		RSAPublicKey:                   appConfigM.RSAPublicKey,
		Version:                        appConfigM.Version,
		WelcomeMessage:                 appConfigM.WelcomeMessage,
		NewUserJoinSystemGroup:         appConfigM.NewUserJoinSystemGroup,
		SearchByPhone:                  appConfigM.SearchByPhone,
		RegisterInviteOn:               appConfigM.RegisterInviteOn,
		SendWelcomeMessageOn:           appConfigM.SendWelcomeMessageOn,
		InviteSystemAccountJoinGroupOn: appConfigM.InviteSystemAccountJoinGroupOn,
		RegisterUserMustCompleteInfoOn: appConfigM.RegisterUserMustCompleteInfoOn,
		ChannelPinnedMessageMaxCount:   appConfigM.ChannelPinnedMessageMaxCount,
		DestroyCoolingOffDays:          appConfigM.DestroyCoolingOffDays,
	}, nil
}

func (s *service) GetShortno() (string, error) {
	shortnoM, err := s.shortnoDB.allocateShortnoAtomic()
	if err != nil {
		return "", err
	}
	if shortnoM == nil {
		return "", errors.New("没有短编号可分配")
	}
	return shortnoM.Shortno, nil
}

func (s *service) SetShortnoUsed(shortno string, business string) error {
	return s.shortnoDB.updateUsed(shortno, 1, business)
}

// 开启生成短编号任务
func runGenShortnoTask(ctx *config.Context) {
	shortnoDB := newShortnoDB(ctx)
	errorSleep := time.Second * 2
	for {
		count, err := shortnoDB.queryVailCount()
		if err != nil {
			time.Sleep(errorSleep) // 错误后退避重试
			continue
		}
		if count < 10000 {
			shortnos := generateNums(ctx.GetConfig().ShortNo.NumLen, 100)
			if len(shortnos) > 0 {
				err = shortnoDB.inserts(shortnos)
				if err != nil {
					ctx.Error("添加短编号失败！", zap.Error(err))
				}
			}
		}
		time.Sleep(time.Second * 30) // 后台任务轮询间隔
	}
}

func generateNums(length int, count int) []string {
	var nums = make([]string, 0, count)

	for i := count; i > 0; i-- {
		max := big.NewInt(1e16)
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic(fmt.Sprintf("crypto/rand failed: %v", err))
		}
		var num = n.Int64()
		nums = append(nums, fmt.Sprintf("%016d", num)[0:length])
	}
	return nums

}

// AppConfigResp returns safe configuration info, excluding sensitive fields such as SuperToken.
type AppConfigResp struct {
	RSAPublicKey                   string
	Version                        int
	WelcomeMessage                 string // 登录欢迎语
	NewUserJoinSystemGroup         int    // 新用户是否加入系统群聊
	SearchByPhone                  int    // 是否可通过手机号搜索
	RegisterInviteOn               int    // 是否开启注册邀请
	SendWelcomeMessageOn           int    // 是否发送登录欢迎语
	InviteSystemAccountJoinGroupOn int    // 是否允许邀请系统账号进入群聊
	RegisterUserMustCompleteInfoOn int    // 是否要求注册用户必须填写完整信息
	ChannelPinnedMessageMaxCount   int    // 频道置顶消息最大数量
	DestroyCoolingOffDays          int    // 注销冷静期天数（默认 7）
}
