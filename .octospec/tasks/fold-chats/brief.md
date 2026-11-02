# Fold chats

## Behavior
Both Recent and Follow menus offer Fold chat / Unfold. Only Recent is filtered. Groups and threads are independent. Aggregate entry sorts by latest message timestamp; mentions stay folded. Preserve mute, follow, pin, draft and active chat. Aggregate unread excludes muted counts but retains a muted dot and mention indicator. Inner list preserves pins. Persist per user/space, without cross-device events. Undo folding; last unfold returns to Recent.

## File map
Backend conversation_ext: dedicated preference table, authenticated space-scoped list/set handlers, existing group/thread authorization. Frontend Service/FoldChatService, bridge/foldChats, features/foldChats, ui/FoldedChatsEntry; shared list adapters only.

## Scope
Chat folding only. No notification behavior changes, follow changes, live cross-device synchronization or deployment.

## Verification
Backend handler authorization/validation and scoped persistence tests; frontend aggregation, Service contract, state and component interaction tests; i18n and diff checks; light/dark and Chinese/English Story coverage.
