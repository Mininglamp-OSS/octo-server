package message

// transactionCommitter is the narrow transaction surface needed to guarantee
// that external side effects run only after a successful database commit.
type transactionCommitter interface {
	Commit() error
}

// commitThenRun commits first and invokes afterCommit only on success. Keep CMD
// sends and other external side effects in afterCommit so a failed transaction
// cannot publish state that was rolled back, and no network call extends the
// transaction's lock-hold window.
func commitThenRun(tx transactionCommitter, afterCommit func()) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	if afterCommit != nil {
		afterCommit()
	}
	return nil
}
