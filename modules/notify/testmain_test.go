package notify

import (
	"os"
	"testing"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl"
	docsaccessrequest "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_access_request"
	docscommented "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_commented"
	docsshared "github.com/Mininglamp-OSS/octo-server/pkg/cardtmpl/docs_shared"
)

// TestMain 为整个 modules/notify 测试包预注册 pilot + roadmap C 新迁移的 L2a 卡,
// 与 main.installCardTmplRegistry() 保持一致。docs.commented / docs.shared 的
// preflight (F7:未注入 = composition bug = 500) 要求 Registry 常驻;个别测试若
// 需空 Registry(如 TestDeliverDocsAccessRequest_UnwiredRegistryFailsClosed)会
// 局部 SetDefaultRegistry(nil) 覆盖。
func TestMain(m *testing.M) {
	registry := cardtmpl.NewRegistry()
	registry.Register(docsaccessrequest.New(), docsaccessrequest.Assets, docsaccessrequest.HandoffRoot)
	registry.Register(docsaccessrequest.NewV3(), docsaccessrequest.Assets, docsaccessrequest.HandoffRootV3)
	registry.SetDefault(docsaccessrequest.TemplateID, docsaccessrequest.TemplateVersionV3)
	registry.Register(docscommented.New(), docscommented.Assets, docscommented.HandoffRoot)
	registry.SetDefault(docscommented.TemplateID, docscommented.TemplateVersion)
	registry.Register(docsshared.New(), docsshared.Assets, docsshared.HandoffRoot)
	registry.SetDefault(docsshared.TemplateID, docsshared.TemplateVersion)
	registry.Freeze()
	cardtmpl.SetDefaultRegistry(registry)
	os.Exit(m.Run())
}
