package user

import (
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/network"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	common "github.com/Mininglamp-OSS/octo-server/modules/common"
	"github.com/Mininglamp-OSS/octo-server/pkg/errcode"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

func (u *User) github(c *wkhttp.Context) {
	cfg := u.ctx.GetConfig()
	authcode := c.Query("authcode")
	redirectURL := fmt.Sprintf("%s%s", cfg.External.APIBaseURL, "/user/oauth/github")
	oauthURL := fmt.Sprintf("%s?client_id=%s&redirect_uri=%s&state=%s", cfg.Github.OAuthURL, cfg.Github.ClientID, url.QueryEscape(redirectURL), authcode)
	c.Redirect(http.StatusFound, oauthURL)
}

// githubOAuth githubOAuth授权
func (u *User) githubOAuth(c *wkhttp.Context) {
	code := c.Query("code")
	if len(code) == 0 {
		respondUserRequestInvalid(c, "code")
		return
	}
	authcode := c.Query("state")
	accessToken, err := u.requestGithubAccessToken(code)
	if err != nil {
		u.Error("获取 github access_token 失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserOAuthExchangeFailed)
		return
	}
	userInfo, err := u.requestGithubUserInfo(accessToken)
	if err != nil {
		u.Error("获取 github 用户信息失败", zap.Error(err))
		respondUserError(c, errcode.ErrUserOAuthProfileFailed)
		return
	}
	if userInfo == nil {
		respondUserError(c, errcode.ErrUserOAuthProfileFailed)
		return
	}
	userInfoM, err := u.db.queryWithGithubUID(userInfo.Login)
	if err != nil {
		u.Error("查询github用户信息失败！", zap.String("login", userInfo.Login), zap.Error(err))
		respondUserError(c, errcode.ErrUserQueryFailed)
		return
	}
	loginSpan := u.ctx.Tracer().StartSpan(
		"giteelogin",
		opentracing.ChildOf(c.GetSpanContext()),
	)

	deviceFlag := config.APP
	loginSpanCtx := u.ctx.Tracer().ContextWithSpan(context.Background(), loginSpan)
	loginSpan.SetTag("username", userInfo.Login)
	defer loginSpan.Finish()

	var loginResp *loginUserDetailResp
	if userInfoM != nil { // 存在就登录
		if userInfoM.IsDestroy == IsDestroyDone {
			respondUserError(c, errcode.ErrUserNotFound)
			return
		}
		loginResp, err = u.execLogin(userInfoM, deviceFlag, nil, loginSpanCtx)
		if err != nil {
			u.respondExecLoginError(c, err, userInfoM)
			return
		}
		// 发送登录消息
		publicIP := util.GetClientPublicIP(c.Request)
		go u.sentWelcomeMsg(publicIP, userInfoM.UID)
	} else {
		if common.EnsureSystemSettings(u.ctx).RegisterOff() {
			// 必须先把 authcode 标记为登录失败再返回,详见 api_gitee.go 同位
			// 注释:thirdAuthcode 起手把 Redis 写成 "1",前端轮询靠下面的
			// SetAndExpire 推进。直接 return 会让 /thirdlogin/authstatus 一直
			// 拿到"等待中"直到 5min TTL 过期。
			if redisErr := u.ctx.GetRedisConn().SetAndExpire(
				fmt.Sprintf("%s%s", ThirdAuthcodePrefix, authcode), "0", time.Minute*1,
			); redisErr != nil {
				u.Error("write authcode failure marker after RegisterOff", zap.Error(redisErr))
			}
			respondUserError(c, errcode.ErrUserRegistrationClosed)
			return
		}
		// 创建用户
		uid := util.GenerUUID()
		name := userInfo.Name
		if strings.TrimSpace(name) == "" {
			name = userInfo.Login
		}
		name = strings.ReplaceAll(name, "@", "_")
		var model = &createUserModel{
			UID:       uid,
			Zone:      "",
			Phone:     "",
			Password:  "",
			Name:      name,
			GithubUID: userInfo.Login,
			Flag:      int(deviceFlag.Uint8()),
		}
		if userInfo.AvatarURL != "" {
			timeoutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			imgReader, _ := u.fileService.DownloadImage(userInfo.AvatarURL, timeoutCtx)
			cancel()
			if imgReader != nil {
				avatarID := crc32.ChecksumIEEE([]byte(uid)) % uint32(u.ctx.GetConfig().Avatar.Partition)
				_, err = u.fileService.UploadFile(fmt.Sprintf("avatar/%d/%s.png", avatarID, uid), "image/png", "", func(w io.Writer) error {
					_, err := io.Copy(w, imgReader)
					return err
				})
				defer imgReader.Close()
				if err == nil {
					model.IsUploadAvatar = 1
				}
			}
		}
		tx, err := u.ctx.DB().Begin()
		if err != nil {
			u.Error("开启事务失败！", zap.Error(err))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
		defer func() {
			if err := recover(); err != nil {
				tx.Rollback()
				fmt.Fprintf(os.Stderr, "recovered panic in goroutine: %v\n%s\n", err, debug.Stack())
			}
		}()

		err = u.githubDB.insertTx(userInfo.toModel(), tx)
		if err != nil {
			tx.Rollback()
			u.Error("插入gitee user失败！", zap.Error(err))
			respondUserError(c, errcode.ErrUserStoreFailed)
			return
		}
		// 发送登录消息
		publicIP := util.GetClientPublicIP(c.Request)
		loginResp, err = u.createUserWithRespAndTx(loginSpanCtx, model, publicIP, nil, tx, func() error {
			err := tx.Commit()
			if err != nil {
				tx.Rollback()
				u.Error("数据库事物提交失败", zap.Error(err))
				respondUserError(c, errcode.ErrUserStoreFailed)
				return nil
			}
			return nil
		})
		if err != nil {
			tx.Rollback()
			u.Error("创建 github 用户失败", zap.Error(err))
			respondUserError(c, errcode.ErrUserRegisterFailed)
			return
		}
	}
	var loginRespStr string
	if loginResp != nil {
		loginRespStr = util.ToJson(loginResp)
	} else {
		loginRespStr = "0"
	}
	err = u.ctx.GetRedisConn().SetAndExpire(fmt.Sprintf("%s%s", ThirdAuthcodePrefix, authcode), loginRespStr, time.Minute*1)
	if err != nil {
		u.Error("redis set error", zap.Error(err))
		respondUserError(c, errcode.ErrUserTokenCacheFailed)
		return
	}
	// 认证结果已存入 Redis，前端通过轮询获取，无需延迟响应
	c.String(http.StatusOK, "登录失败！") // 如果一切正常，理论上是看不到这个返回结果的
}

func (u *User) requestGithubAccessToken(code string) (string, error) {
	cfg := u.ctx.GetConfig()

	result, err := network.PostForWWWForm("https://github.com/login/oauth/access_token", map[string]string{
		"code":          code,
		"client_id":     cfg.Github.ClientID,
		"redirect_uri":  fmt.Sprintf("%s%s", cfg.External.APIBaseURL, "/user/oauth/github"),
		"client_secret": cfg.Github.ClientSecret,
	}, map[string]string{
		"Accept": "application/json",
	})
	if err != nil {
		return "", err
	}

	accessToken := ""
	if result["access_token"] != nil {
		if token, ok := result["access_token"].(string); ok {
			accessToken = token
		} else {
			return "", errors.New("access_token 类型错误")
		}
	}

	return accessToken, nil
}

func (u *User) requestGithubUserInfo(accessToken string) (*githubUser, error) {
	userInfo := &githubUser{}
	resp, err := network.Get("https://api.github.com/user", nil, map[string]string{
		"Accept":               "application/vnd.github+json",
		"Authorization":        fmt.Sprintf("Bearer %s", accessToken),
		"X-GitHub-Api-Version": "2022-11-28",
	})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.Errorf("获取gitee用户信息失败，状态码：%d", resp.StatusCode)
	}
	err = util.ReadJsonByByte([]byte(resp.Body), &userInfo)
	if err != nil {
		return nil, err
	}
	return userInfo, nil
}

type githubUser struct {
	ID                      int64      `json:"id"`
	Login                   string     `json:"login"`
	NodeID                  string     `json:"node_id"`
	AvatarURL               string     `json:"avatar_url"`
	GravatarID              string     `json:"gravatar_id"`
	URL                     string     `json:"url"`
	HtmlUrl                 string     `json:"html_url"`
	FollowersURL            string     `json:"followers_url"`
	FollowingURL            string     `json:"following_url"`
	GistsURL                string     `json:"gists_url"`
	StarredURL              string     `json:"starred_url"`
	SubscriptionsURL        string     `json:"subscriptions_url"`
	OrganizationsURL        string     `json:"organizations_url"`
	ReposURL                string     `json:"repos_url"`
	EventsURL               string     `json:"events_url"`
	ReceivedEventsURL       string     `json:"received_events_url"`
	Type                    string     `json:"type"`
	SiteAdmin               bool       `json:"site_admin"`
	Name                    string     `json:"name"`
	Company                 string     `json:"company"`
	Blog                    string     `json:"blog"`
	Location                string     `json:"location"`
	Email                   string     `json:"email"`
	Hireable                bool       `json:"hireable"`
	Bio                     string     `json:"bio"`
	TwitterUsername         string     `json:"twitter_username"`
	PublicRepos             int        `json:"public_repos"`
	PublicGists             int        `json:"public_gists"`
	Followers               int        `json:"followers"`
	Following               int        `json:"following"`
	CreatedAt               string     `json:"created_at"`
	UpdatedAt               string     `json:"updated_at"`
	PrivateGists            int        `json:"private_gists"`
	TotalPrivateRepos       int        `json:"total_private_repos"`
	OwnedPrivateRepos       int        `json:"owned_private_repos"`
	DiskUsage               int        `json:"disk_usage"`
	Collaborators           int        `json:"collaborators"`
	TwoFactorAuthentication bool       `json:"two_factor_authentication"`
	Plan                    githubPlan `json:"plan"`
}
type githubPlan struct {
	Name          string `json:"name"`
	Space         int    `json:"space"`
	PrivateRepos  int    `json:"private_repos"`
	Collaborators int    `json:"collaborators"`
}

func (g *githubUser) toModel() *githubUserInfoModel {

	m := &githubUserInfoModel{
		ID:                      g.ID,
		Login:                   g.Login,
		NodeID:                  g.NodeID,
		AvatarURL:               g.AvatarURL,
		GravatarID:              g.GravatarID,
		URL:                     g.URL,
		HtmlUrl:                 g.HtmlUrl,
		FollowersURL:            g.FollowersURL,
		FollowingURL:            g.FollowingURL,
		GistsURL:                g.GistsURL,
		StarredURL:              g.StarredURL,
		SubscriptionsURL:        g.SubscriptionsURL,
		OrganizationsURL:        g.OrganizationsURL,
		ReposURL:                g.ReposURL,
		EventsURL:               g.EventsURL,
		ReceivedEventsURL:       g.ReceivedEventsURL,
		Type:                    g.Type,
		SiteAdmin:               g.SiteAdmin,
		Name:                    g.Name,
		Company:                 g.Company,
		Blog:                    g.Blog,
		Location:                g.Location,
		Email:                   g.Email,
		Hireable:                g.Hireable,
		Bio:                     g.Bio,
		TwitterUsername:         g.TwitterUsername,
		PublicRepos:             g.PublicRepos,
		PublicGists:             g.PublicGists,
		Followers:               g.Followers,
		Following:               g.Following,
		GithubCreatedAt:         g.CreatedAt,
		GithubUpdatedAt:         g.UpdatedAt,
		PrivateGists:            g.PrivateGists,
		TotalPrivateRepos:       g.TotalPrivateRepos,
		OwnedPrivateRepos:       g.OwnedPrivateRepos,
		DiskUsage:               g.DiskUsage,
		Collaborators:           g.Collaborators,
		TwoFactorAuthentication: g.TwoFactorAuthentication,
	}

	return m
}
