package space

import (
	"github.com/Mininglamp-OSS/octo-server/pkg/permissionbundle"
	"github.com/gocraft/dbr/v2"
)

func (d *DB) commitMemberTx(tx *dbr.Tx, spaceID, uid string, newAssociation bool) error {
	return permissionbundle.CommitAssociations(d.ctx, tx, permissionbundle.Association{SpaceID: spaceID, BotUID: uid, New: newAssociation})
}
