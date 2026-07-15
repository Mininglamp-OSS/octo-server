package notify

import (
	"github.com/Mininglamp-OSS/octo-lib/config"
	"github.com/Mininglamp-OSS/octo-server/internal/cardactiondispatch"
	"github.com/Mininglamp-OSS/octo-server/internal/carddispatch"
)

// NewActionFinalizerFromContext composes the standard approval fallback with
// the docs-specific visual. New standard consumers require no code binding.
func NewActionFinalizerFromContext(ctx *config.Context) (*cardactiondispatch.FinalizerRegistry, error) {
	docs, err := NewDocsActionFinalizerFromContext(ctx)
	if err != nil {
		return nil, err
	}
	outcomeSender, err := carddispatch.SenderFromContext(ctx, actionOutcomeProducerID)
	if err != nil {
		return nil, err
	}
	standard, err := NewStandardActionFinalizer(carddispatch.NewCardMutator(ctx), outcomeSender)
	if err != nil {
		return nil, err
	}
	return cardactiondispatch.NewFinalizerRegistry(standard, map[cardactiondispatch.FinalizerKey]cardactiondispatch.Finalizer{
		{Owner: "docs", ActionType: "access_request.decision"}: docs,
	})
}
