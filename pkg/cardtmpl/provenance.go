package cardtmpl

// PR-C D3（spec: .octospec/tasks/cardtmpl-runtime-catalog-grants-discovery/
// brief.md）：stored server-authored provenance 与 catalog principal 的唯一
// 桥接。wire 形状与 strict parse 在 pkg/cardmsg/provenance.go（信封权威层，
// carddispatch 也要写标记而 cardtmpl 依赖 carddispatch，值对象放这里会成环）；
// 本文件只做 typed principal 映射 —— historical_edit / action_context 的
// principal 输入从此来自 stored marker，而不是 sender/owner 猜测。

import (
	"fmt"

	"github.com/Mininglamp-OSS/octo-server/pkg/cardmsg"
)

// CatalogPrincipalFromProvenance 把一个已存储的 wire marker 映射为 catalog
// principal。marker 先过 strict Validate；wire principal 只有 bot 与
// internal_producer 两种（space 是 discover-only grant principal、system 是
// 内部保留值，都不可能出现在存储帧里），未知类型 fail-close。
func CatalogPrincipalFromProvenance(provenance cardmsg.CatalogProvenance) (CatalogPrincipal, error) {
	if err := provenance.Validate(); err != nil {
		return CatalogPrincipal{}, err
	}
	var kind CatalogPrincipalKind
	switch provenance.PrincipalType {
	case cardmsg.CatalogPrincipalWireBot:
		kind = CatalogPrincipalBot
	case cardmsg.CatalogPrincipalWireInternalProducer:
		kind = CatalogPrincipalInternalProducer
	default:
		return CatalogPrincipal{}, fmt.Errorf("%w: principal_type %q is not a catalog principal",
			cardmsg.ErrCatalogMarkerInvalid, provenance.PrincipalType)
	}
	return CatalogPrincipal{Kind: kind, ID: provenance.PrincipalID, SpaceID: provenance.SpaceID}, nil
}
