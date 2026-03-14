package space

import (
	"fmt"

	"time"

	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/util"
	"github.com/Mininglamp-OSS/octo-server/pkg/network"
	"go.uber.org/zap"

	spacepkg "github.com/Mininglamp-OSS/octo-server/pkg/space"
)

const (
	// Redis cache keys
	CacheKeySpaceMembers = "space:members:"   // SET space:members:{spaceId}
	CacheKeyBotsAll      = "bots:all"         // SET bots:all

	// Cache TTL
	CacheSpaceMembersTTL = 10 * time.Minute
	CacheBotsTTL         = 30 * time.Minute
)

// PermissionService handles Space permission checking
type PermissionService struct {
	ctx *config.Context
	db  *DB
	log.Log
}

// NewPermissionService creates a new PermissionService
func NewPermissionService(ctx *config.Context) *PermissionService {
	return &PermissionService{
		ctx: ctx,
		db:  NewDB(ctx),
		Log: log.NewTLog("SpacePermission"),
	}
}

// CheckSpacePermissionReq request for checking space permission
type CheckSpacePermissionReq struct {
	FromUID     string `json:"from_uid"`
	ToChannelID string `json:"to_channel_id"`
}

// CheckSpacePermissionResp response for space permission check
type CheckSpacePermissionResp struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
}

// parseSpaceChannelID parses a Space channel_id (format: s{spaceId}_{uid})
// Uses spacepkg.ParseChannelID which supports known SpaceID prefix matching
func parseSpaceChannelID(channelID string) (spaceID string, uid string, isSpaceChannel bool) {
	spaceID, uid = spacepkg.ParseChannelID(channelID)
	return spaceID, uid, spaceID != ""
}

// CheckSpacePermission checks if fromUID can send message to toChannelID
func (s *PermissionService) CheckSpacePermission(req *CheckSpacePermissionReq) (*CheckSpacePermissionResp, error) {
	// Parse fromUID
	fromSpaceID, fromUID, fromIsSpace := parseSpaceChannelID(req.FromUID)
	// Parse toChannelID
	toSpaceID, toUID, toIsSpace := parseSpaceChannelID(req.ToChannelID)

	s.Debug("CheckSpacePermission",
		zap.String("fromUID", req.FromUID),
		zap.String("toChannelID", req.ToChannelID),
		zap.String("fromSpaceID", fromSpaceID),
		zap.String("fromRealUID", fromUID),
		zap.String("toSpaceID", toSpaceID),
		zap.String("toRealUID", toUID),
		zap.Bool("fromIsSpace", fromIsSpace),
		zap.Bool("toIsSpace", toIsSpace),
	)

	// Check if both are bots
	fromIsBot, err := s.isBot(fromUID)
	if err != nil {
		return nil, err
	}
	toIsBot, err := s.isBot(toUID)
	if err != nil {
		return nil, err
	}

	// Rule: bot + bot -> always deny
	if fromIsBot && toIsBot {
		return &CheckSpacePermissionResp{Allow: false, Reason: "bot_to_bot"}, nil
	}

	// Determine the target Space
	// fromUID may not have Space prefix (e.g. from WebSocket CONNECT, fromUID is bare UID)
	// In that case, use toSpaceID as the Space context
	targetSpaceID := ""
	if toIsSpace {
		targetSpaceID = toSpaceID
	}
	if fromIsSpace && targetSpaceID != "" {
		// Both have Space prefix - check if same Space
		if fromSpaceID != targetSpaceID {
			// Different Space
			if fromIsBot || toIsBot {
				return &CheckSpacePermissionResp{Allow: false, Reason: "cross_space_bot"}, nil
			}
			isFriend, err := s.isFriend(fromUID, toUID)
			if err != nil {
				return nil, err
			}
			if isFriend {
				return &CheckSpacePermissionResp{Allow: true}, nil
			}
			return &CheckSpacePermissionResp{Allow: false, Reason: "not_friend"}, nil
		}
	}
	sameSpace := targetSpaceID != ""

	if sameSpace {
		// Same Space scenario
		if fromIsBot || toIsBot {
			// human + bot in same Space -> check friend relationship
			isFriend, err := s.isFriend(fromUID, toUID)
			if err != nil {
				return nil, err
			}
			if isFriend {
				return &CheckSpacePermissionResp{Allow: true}, nil
			}
			return &CheckSpacePermissionResp{Allow: false, Reason: "not_friend"}, nil
		}
		// human + human in same Space -> check both are members
		fromIsMember, err := s.isSpaceMember(targetSpaceID, fromUID)
		if err != nil {
			return nil, err
		}
		toIsMember, err := s.isSpaceMember(targetSpaceID, toUID)
		if err != nil {
			return nil, err
		}
		if fromIsMember && toIsMember {
			return &CheckSpacePermissionResp{Allow: true}, nil
		}
		return &CheckSpacePermissionResp{Allow: false, Reason: "not_both_members"}, nil
	}

	// No Space context (bare UIDs) or different Space
	if (fromIsBot || toIsBot) && (fromIsSpace || toIsSpace) {
		// Explicitly different Space with bot involved -> reject
		return &CheckSpacePermissionResp{Allow: false, Reason: "cross_space_bot"}, nil
	}

	// Bare UIDs (no Space prefix) - check for common Space first
	if !fromIsSpace && !toIsSpace {
		hasCommon, err := s.hasCommonSpace(fromUID, toUID)
		if err != nil {
			return nil, err
		}
		if hasCommon {
			// Same Space members
			if fromIsBot || toIsBot {
				// human + bot in same Space -> check friend relationship
				isFriend, err := s.isFriend(fromUID, toUID)
				if err != nil {
					return nil, err
				}
				if isFriend {
					return &CheckSpacePermissionResp{Allow: true}, nil
				}
				return &CheckSpacePermissionResp{Allow: false, Reason: "not_friend"}, nil
			}
			// human + human in same Space -> allow
			return &CheckSpacePermissionResp{Allow: true}, nil
		}
	}

	// No common Space -> check friend relationship
	isFriend, err := s.isFriend(fromUID, toUID)
	if err != nil {
		return nil, err
	}
	if isFriend {
		return &CheckSpacePermissionResp{Allow: true}, nil
	}
	return &CheckSpacePermissionResp{Allow: false, Reason: "not_friend"}, nil
}

// isBot checks if uid is an active bot with Redis cache
func (s *PermissionService) isBot(uid string) (bool, error) {
	if uid == "" {
		return false, nil
	}

	redis := s.ctx.GetRedisConn()
	isMember, err := redis.Sismember(CacheKeyBotsAll, uid)
	if err != nil {
		s.Warn("Redis Sismember failed for bots cache", zap.Error(err), zap.String("uid", uid))
		// Fall through to DB query
	} else if isMember > 0 {
		return true, nil
	}

	// Check if cache exists - if not, rebuild it
	members, err := redis.SMembers(CacheKeyBotsAll)
	if err != nil {
		s.Warn("Redis SMembers failed for bots cache", zap.Error(err))
	}
	if len(members) == 0 {
		// Rebuild cache from DB
		if err := s.rebuildBotCache(); err != nil {
			s.Warn("Failed to rebuild bot cache", zap.Error(err))
		}
		// Re-check after rebuild
		isMember, err = redis.Sismember(CacheKeyBotsAll, uid)
		if err == nil && isMember > 0 {
			return true, nil
		}
	}

	// Fallback to DB query
	return s.db.isRobot(uid)
}

// isSpaceMember checks if uid is a member of the space with Redis cache
func (s *PermissionService) isSpaceMember(spaceID, uid string) (bool, error) {
	if spaceID == "" || uid == "" {
		return false, nil
	}

	cacheKey := CacheKeySpaceMembers + spaceID
	redis := s.ctx.GetRedisConn()

	isMember, err := redis.Sismember(cacheKey, uid)
	if err != nil {
		s.Warn("Redis Sismember failed for space members cache", zap.Error(err), zap.String("spaceID", spaceID), zap.String("uid", uid))
		// Fall through to DB query
	} else if isMember > 0 {
		return true, nil
	}

	// Check if cache exists - if not, rebuild it
	members, err := redis.SMembers(cacheKey)
	if err != nil {
		s.Warn("Redis SMembers failed for space members cache", zap.Error(err))
	}
	if len(members) == 0 {
		// Rebuild cache from DB
		if err := s.rebuildSpaceMemberCache(spaceID); err != nil {
			s.Warn("Failed to rebuild space member cache", zap.Error(err), zap.String("spaceID", spaceID))
		}
		// Re-check after rebuild
		isMember, err = redis.Sismember(cacheKey, uid)
		if err == nil && isMember > 0 {
			return true, nil
		}
	}

	// Fallback to DB query
	member, err := s.db.queryMember(spaceID, uid)
	if err != nil {
		return false, err
	}
	return member != nil, nil
}

// hasCommonSpace checks if two users share at least one Space
func (s *PermissionService) hasCommonSpace(uid1, uid2 string) (bool, error) {
	if uid1 == "" || uid2 == "" {
		return false, nil
	}
	spaceID := GetCommonSpaceID(s.ctx, uid1, uid2)
	return spaceID != "", nil
}

// isFriend checks if two users are friends
func (s *PermissionService) isFriend(uid1, uid2 string) (bool, error) {
	if uid1 == "" || uid2 == "" {
		return false, nil
	}
	var isFriend bool
	err := s.ctx.DB().Select("COUNT(*)>0").From("friend").
		Where("uid=? AND to_uid=? AND is_deleted=0", uid1, uid2).
		LoadOne(&isFriend)
	if err != nil {
		return false, err
	}
	return isFriend, nil
}

// isRobot checks if uid is an active bot in DB
func (d *DB) isRobot(uid string) (bool, error) {
	var count int
	err := d.session.Select("COUNT(*)").From("robot").Where("robot_id=? AND status=1", uid).LoadOne(&count)
	return count > 0, err
}

// rebuildBotCache rebuilds the bots:all cache from DB
func (s *PermissionService) rebuildBotCache() error {
	var robotIDs []string
	_, err := s.ctx.DB().Select("robot_id").From("robot").Where("status=1").Load(&robotIDs)
	if err != nil {
		return err
	}

	redis := s.ctx.GetRedisConn()
	// Delete old cache
	_ = redis.Del(CacheKeyBotsAll)
	// Add all bots to cache
	if len(robotIDs) > 0 {
		members := make([]interface{}, len(robotIDs))
		for i, id := range robotIDs {
			members[i] = id
		}
		err = redis.SAdd(CacheKeyBotsAll, members...)
		if err != nil {
			return err
		}
	}
	// Set expiration
	err = redis.SetExpire(CacheKeyBotsAll, CacheBotsTTL)
	if err != nil {
		s.Warn("Failed to set expire for bots cache", zap.Error(err))
	}

	s.Debug("Rebuilt bots cache", zap.Int("count", len(robotIDs)))
	return nil
}

// rebuildSpaceMemberCache rebuilds the space members cache from DB
func (s *PermissionService) rebuildSpaceMemberCache(spaceID string) error {
	uids, err := GetSpaceMemberUIDs(s.ctx, spaceID)
	if err != nil {
		return err
	}

	cacheKey := CacheKeySpaceMembers + spaceID
	redis := s.ctx.GetRedisConn()
	// Delete old cache
	_ = redis.Del(cacheKey)
	// Add all members to cache
	if len(uids) > 0 {
		members := make([]interface{}, len(uids))
		for i, uid := range uids {
			members[i] = uid
		}
		err = redis.SAdd(cacheKey, members...)
		if err != nil {
			return err
		}
	}
	// Set expiration
	err = redis.SetExpire(cacheKey, CacheSpaceMembersTTL)
	if err != nil {
		s.Warn("Failed to set expire for space members cache", zap.Error(err), zap.String("spaceID", spaceID))
	}

	s.Debug("Rebuilt space members cache", zap.String("spaceID", spaceID), zap.Int("count", len(uids)))
	return nil
}

// AddSpaceMemberToCache adds a member to the space cache
func AddSpaceMemberToCache(ctx *config.Context, spaceID, uid string) error {
	cacheKey := CacheKeySpaceMembers + spaceID
	redis := ctx.GetRedisConn()
	if err := redis.SAdd(cacheKey, uid); err != nil {
		return err
	}
	_ = redis.SetExpire(cacheKey, CacheSpaceMembersTTL)
	return nil
}

// RemoveSpaceMemberFromCache removes a member from the space cache
func RemoveSpaceMemberFromCache(ctx *config.Context, spaceID, uid string) error {
	cacheKey := CacheKeySpaceMembers + spaceID
	return ctx.GetRedisConn().SRem(cacheKey, uid)
}

// AddBotToCache adds a bot to the bots cache
func AddBotToCache(ctx *config.Context, uid string) error {
	redis := ctx.GetRedisConn()
	if err := redis.SAdd(CacheKeyBotsAll, uid); err != nil {
		return err
	}
	_ = redis.SetExpire(CacheKeyBotsAll, CacheBotsTTL)
	return nil
}

// RemoveBotFromCache removes a bot from the bots cache
func RemoveBotFromCache(ctx *config.Context, uid string) error {
	return ctx.GetRedisConn().SRem(CacheKeyBotsAll, uid)
}

// FlushWuKongIMPermissionCache notifies WuKongIM to flush permission cache for given UIDs
func FlushWuKongIMPermissionCache(ctx *config.Context, uids []string) error {
	if len(uids) == 0 {
		return nil
	}

	apiURL := ctx.GetConfig().WuKongIM.APIURL
	if apiURL == "" {
		return nil
	}

	url := fmt.Sprintf("%s/permission/cache/flush", apiURL)
	body := util.ToJson(map[string]interface{}{
		"uids": uids,
	})

	headers := map[string]string{
		"Content-Type": "application/json",
	}
	// Add manager token if configured
	if ctx.GetConfig().WuKongIM.ManagerToken != "" {
		headers["token"] = ctx.GetConfig().WuKongIM.ManagerToken
	}

	resp, err := network.Post(url, []byte(body), headers)
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("WuKongIM permission cache flush failed: status=%d, body=%s", resp.StatusCode, resp.Body)
	}
	return nil
}

// FlushWuKongIMPermissionCacheAsync asynchronously notifies WuKongIM to flush permission cache
func FlushWuKongIMPermissionCacheAsync(ctx *config.Context, uids []string) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.NewTLog("SpacePermission").Error("FlushWuKongIMPermissionCacheAsync panic", zap.Any("recover", r))
			}
		}()
		if err := FlushWuKongIMPermissionCache(ctx, uids); err != nil {
			log.NewTLog("SpacePermission").Warn("Failed to flush WuKongIM permission cache", zap.Error(err), zap.Strings("uids", uids))
		}
	}()
}
