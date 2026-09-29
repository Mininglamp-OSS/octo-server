package imreconcile

import (
	"strings"

	"github.com/Mininglamp-OSS/octo-lib/config"
)

func managedGroup(channel string, channelType uint8) (string, bool) {
	if !Enabled() {
		return "", false
	}
	if channelType == 2 {
		return channel, true
	}
	if channelType == 5 {
		group, _, ok := strings.Cut(channel, "____")
		return group, ok && group != ""
	}
	return "", false
}

// Membership and creation flags come from the committed business database,
// never from a caller's potentially stale add/remove slice or creation flags.
func CreateChannel(ctx *config.Context, req *config.ChannelCreateReq) error {
	if group, ok := managedGroup(req.ChannelID, req.ChannelType); ok {
		return Flush(ctx, group)
	}
	return ctx.IMCreateOrUpdateChannel(req)
}

func AddSubscribers(ctx *config.Context, req *config.SubscriberAddReq) error {
	if group, ok := managedGroup(req.ChannelID, req.ChannelType); ok {
		return Flush(ctx, group)
	}
	return ctx.IMAddSubscriber(req)
}

func RemoveSubscribers(ctx *config.Context, req *config.SubscriberRemoveReq) error {
	if group, ok := managedGroup(req.ChannelID, req.ChannelType); ok {
		return Flush(ctx, group)
	}
	return ctx.IMRemoveSubscriber(req)
}

func AddDenylist(ctx *config.Context, req config.ChannelBlacklistReq) error {
	if group, ok := managedGroup(req.ChannelID, req.ChannelType); ok {
		return Flush(ctx, group)
	}
	return ctx.IMBlacklistAdd(req)
}

func RemoveDenylist(ctx *config.Context, req config.ChannelBlacklistReq) error {
	if group, ok := managedGroup(req.ChannelID, req.ChannelType); ok {
		return Flush(ctx, group)
	}
	return ctx.IMBlacklistRemove(req)
}

func UpdateChannelInfo(ctx *config.Context, req *config.ChannelInfoCreateReq) error {
	if group, ok := managedGroup(req.ChannelID, req.ChannelType); ok {
		return Flush(ctx, group)
	}
	return ctx.IMCreateOrUpdateChannelInfo(req)
}
