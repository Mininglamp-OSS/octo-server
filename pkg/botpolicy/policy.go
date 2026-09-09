// Package botpolicy defines server-owned Bot kinds and collaboration policies.
// Runtime hosting reports and creator absence never grant a capability.
package botpolicy

type Kind string

const (
	User          Kind = "user"
	App           Kind = "app"
	Avatar        Kind = "avatar"
	ScopePlatform      = "platform"
	ScopeSpace         = "space"
	Draft              = "draft"
	Published          = "published"
	Unpublished        = "unpublished"
	Deleted            = "deleted"
)

type Capability string

const (
	Message        Capability = "message"
	ReadGroup      Capability = "group.read"
	ManageGroup    Capability = "group.manage"
	ReadThread     Capability = "thread.read"
	ManageThread   Capability = "thread.manage"
	File           Capability = "file"
	Card           Capability = "card"
	Commands       Capability = "commands"
	Runtime        Capability = "runtime"
	Search         Capability = "search"
	Voice          Capability = "voice"
	OBO            Capability = "obo"
	SpacePrincipal Capability = "space.principal"
)

// Allows is an allowlist: new kinds/capabilities are denied until reviewed.
func Allows(kind Kind, capability Capability) bool {
	switch capability {
	case Message, File, Card, Commands, Runtime:
		return kind == User || kind == App || kind == Avatar
	case ReadGroup, ReadThread:
		return kind == User || kind == Avatar
	case ManageGroup, ManageThread, Search, Voice, OBO, SpacePrincipal:
		return kind == User
	default:
		return false
	}
}

// ActiveAvatarSQL and TeamEligibilitySQL accept only source-code SQL aliases
// and expressions, never request values. Parameters remain placeholders.
func ActiveAvatarSQL(robot string) string {
	return "(" + robot + ".kind='avatar' AND " + robot + ".status=1 AND " + robot + ".creator_uid='' AND " + robot + ".publication_state='published' AND " + robot + ".lifecycle_pending=0 AND ((" + robot + ".management_scope='platform' AND " + robot + ".management_space_id='') OR (" + robot + ".management_scope='space' AND " + robot + ".management_space_id<>'')))"
}

func AvatarInSpaceSQL(robot, space string) string {
	return "(" + ActiveAvatarSQL(robot) + " AND (" + robot + ".management_scope='platform' OR (" + robot + ".management_scope='space' AND " + robot + ".management_space_id=" + space + ")))"
}

// TeamEligibilitySQL is entitlement only: callers must additionally require
// live accounts, an active Space and both human/Bot space_member seats.
func TeamEligibilitySQL(robot, owner, space string) string {
	return "((" + robot + ".kind='user' AND " + robot + ".creator_uid=" + owner + ") OR " + AvatarInSpaceSQL(robot, space) + ")"
}
