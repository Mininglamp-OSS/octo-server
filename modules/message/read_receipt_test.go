package message

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-lib/common"
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-lib/pkg/log"
	"github.com/Mininglamp-OSS/octo-lib/pkg/wkhttp"
	"github.com/Mininglamp-OSS/octo-server/internal/readreceipttest"
	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/gocraft/dbr/v2"
	"github.com/gocraft/dbr/v2/dialect"
)

func TestMessageReadedCommitGatesCache(t *testing.T) {
	for _, channelType := range []uint8{1, 2} {
		for _, outcome := range []string{"retry-success", "exhausted", "commit-unknown"} {
			t.Run(outcome+string(rune('0'+channelType)), func(t *testing.T) {
				cache := readreceipttest.NewCache(t)
				cache.ExpectCount(0)
				if outcome == "retry-success" {
					cache.ExpectCount(1)
				}
				im := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.WriteString(w, `{"messages":[{"message_id":7,"message_seq":1,"from_uid":"sender"}]}`)
				}))
				defer im.Close()
				cfg := config.New()
				cfg.WuKongIM.APIURL = im.URL
				cfg.DB.RedisAddr = cache.Addr
				ctx := config.NewContext(cfg)
				defer ctx.Tracer().Close()
				t.Cleanup(func() { ctx.GetRedisConn().Close() })
				db, mock, err := sqlmock.New()
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				db.SetMaxOpenConns(2)
				conn := &dbr.Connection{DB: db, Dialect: dialect.MySQL, EventReceiver: &dbr.NullEventReceiver{}}
				m := &Message{ctx: ctx, Log: log.NewTLog("receipt-test"), memberReadedDB: &memberReadedDB{session: conn.NewSession(nil)}}
				channel := "peer"
				if channelType == 1 {
					channel = common.GetFakeChannelIDWith("peer", "reader")
				}
				pattern := "INSERT INTO member_readed .*" + regexp.QuoteMeta("(7, '"+channel+"', "+strconv.Itoa(int(channelType))+", 'reader')")
				failures := 1
				if outcome == "exhausted" {
					failures = 3
				}
				if outcome == "commit-unknown" {
					failures = 0
				}
				for i := 0; i < failures; i++ {
					mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
					mock.ExpectExec(pattern).WillReturnError(&mysql.MySQLError{Number: 1213})
					mock.ExpectExec("^ROLLBACK$").WillReturnResult(sqlmock.NewResult(0, 0))
				}
				if outcome != "exhausted" {
					mock.ExpectExec("^START TRANSACTION$").WillReturnResult(sqlmock.NewResult(0, 0))
					mock.ExpectExec(pattern).WillReturnResult(sqlmock.NewResult(0, 1))
					if outcome == "commit-unknown" {
						mock.ExpectExec("^COMMIT$").WillReturnResult(sqlmock.NewResult(0, 0)).WillReturnError(io.ErrUnexpectedEOF)
					} else {
						mock.ExpectExec("^COMMIT$").WillReturnResult(sqlmock.NewResult(0, 0))
					}
				}
				body, _ := json.Marshal(map[string]interface{}{"channel_id": "peer", "channel_type": channelType, "message_ids": []string{"7"}})
				rec := httptest.NewRecorder()
				gc, _ := gin.CreateTestContext(rec)
				gc.Request = httptest.NewRequest(http.MethodPost, "/v1/message/readed", bytes.NewReader(body))
				gc.Request.Header.Set("Content-Type", "application/json")
				gc.Set("uid", "reader")
				m.messageReaded(&wkhttp.Context{Context: gc})
				if outcome == "retry-success" {
					if rec.Code != 200 {
						t.Fatalf("%d: %s", rec.Code, rec.Body.String())
					}
					args := cache.WaitSet(t)
					if args[1] != "readedCount:7" || !strings.Contains(args[2], channel) {
						t.Fatalf("bad cache payload: %v", args)
					}
					if cache.Count() != 1 {
						t.Fatalf("cache writes=%d", cache.Count())
					}
				} else {
					if rec.Code == 200 || cache.Count() != 0 {
						t.Fatalf("failed write reported success/cache: %d %s count=%d", rec.Code, rec.Body.String(), cache.Count())
					}
				}
				cache.AssertQuiet(t)
				if err := mock.ExpectationsWereMet(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
