package user

import (
	"fmt"
	"hash/crc32"
)

func userAvatarFilePath(uid string, partition int, version int64) string {
	avatarID := crc32.ChecksumIEEE([]byte(uid)) % uint32(partition)
	if version > 0 {
		return fmt.Sprintf("avatar/%d/%s/%d.png", avatarID, uid, version)
	}
	return fmt.Sprintf("avatar/%d/%s.png", avatarID, uid)
}
