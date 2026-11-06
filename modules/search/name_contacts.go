package search

import (
	"database/sql"
	"strings"

	"github.com/Mininglamp-OSS/octo-server/pkg/namesearch"
)

type nameContactRow struct {
	UID            string        `db:"uid"`
	Name           string        `db:"name"`
	RealName       string        `db:"real_name"`
	Remark         string        `db:"remark"`
	Robot          sql.NullInt64 `db:"robot"`
	Member         bool          `db:"is_member"`
	Friend         bool          `db:"friend"`
	OrdinaryActive bool          `db:"ordinary_active"`
	AppID          string        `db:"app_id"`
	AppName        string        `db:"app_name"`
	AppScope       string        `db:"app_scope"`
	AppSpace       string        `db:"app_space"`
	AppStatus      int           `db:"app_status"`
}

func eligibleNameContacts(rows []nameContactRow, uid, space string) nameCandidateSet {
	return eligibleNameContactsInScope(rows, uid, space, false)
}

// authorizedRoster only skips the directory relationship test for confirmed
// humans already authorized by an operation's SQL roster. Bot checks always run.
func eligibleNameContactsInScope(rows []nameContactRow, uid, space string, authorizedRoster bool) nameCandidateSet {
	set := nameCandidateSet{Complete: true, Candidates: []namesearch.Candidate{}}
	seen := map[string]bool{}
	for _, r := range rows {
		if r.UID == "" || r.UID == uid || seen[r.UID] {
			continue
		}
		if !r.Robot.Valid || (r.Robot.Int64 != 0 && r.Robot.Int64 != 1) {
			set.Complete = false
			continue
		}
		typ, bot := "human", ""
		if r.AppID != "" {
			// App identity wins over every membership/friend source, even if the
			// user row is temporarily inconsistent. Unpublished apps cannot reenter.
			if r.AppStatus != 1 {
				continue
			}
			typ = "ai"
			switch r.AppScope {
			case "platform":
				bot = "platform_app"
			case "space":
				if space == "" || nameFoldID(r.AppSpace) != nameFoldID(space) {
					continue
				}
				bot = "space_app"
			default:
				continue
			}
		} else if r.Robot.Int64 == 1 {
			if space == "" || !r.Member || !r.OrdinaryActive {
				continue
			}
			typ = "ai"
			bot = "ordinary"
		} else if !authorizedRoster && !r.Friend && (space == "" || !r.Member) {
			continue
		}
		name := r.Name
		if r.AppID != "" && strings.TrimSpace(r.AppName) != "" {
			name = r.AppName
		}
		if strings.TrimSpace(name) == "" {
			name = r.RealName
		}
		remark := r.Remark
		if strings.TrimSpace(remark) == "" {
			remark = ""
		}
		set.Candidates = append(set.Candidates, namesearch.Candidate{ID: r.UID, Kind: "contact", ContactType: typ, BotKind: bot, OriginalName: name, RemarkName: remark})
		seen[r.UID] = true
	}
	if !set.Complete {
		set.Reasons = []string{"type_pending"}
	}
	return set
}
