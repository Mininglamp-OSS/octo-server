package botfather

import (
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func TestQueryRobotByBotTokenUsesExactCredentialComparison(t *testing.T) {
	rawDB, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer rawDB.Close()
	conn := &dbr.Connection{DB: rawDB, EventReceiver: &dbr.NullEventReceiver{}, Dialect: dialect.MySQL}
	db := &botfatherDB{session: conn.NewSession(nil)}

	mock.ExpectQuery("SELECT \\* FROM robot WHERE .*BINARY bot_token=BINARY").
		WillReturnRows(sqlmock.NewRows([]string{"robot_id", "creator_uid", "bot_token", "status"}).
			AddRow("bot-1", "human-1", "bf_ExactToken", 1))

	bot, err := db.queryRobotByBotToken("bf_ExactToken")
	if err != nil {
		t.Fatal(err)
	}
	if bot == nil || bot.RobotID != "bot-1" {
		t.Fatalf("bot=%+v", bot)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
