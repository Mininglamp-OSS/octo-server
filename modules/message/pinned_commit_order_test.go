package message

import (
	"errors"
	"reflect"
	"testing"
)

type recordingCommitter struct {
	err   error
	order *[]string
}

func (c recordingCommitter) Commit() error {
	*c.order = append(*c.order, "commit")
	return c.err
}

func TestCommitThenRunRunsCallbackAfterSuccessfulCommit(t *testing.T) {
	order := make([]string, 0, 2)
	err := commitThenRun(recordingCommitter{order: &order}, func() {
		order = append(order, "notify")
	})
	if err != nil {
		t.Fatalf("commitThenRun: %v", err)
	}
	if want := []string{"commit", "notify"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order=%v want %v", order, want)
	}
}

func TestCommitThenRunSkipsCallbackWhenCommitFails(t *testing.T) {
	commitErr := errors.New("commit failed")
	order := make([]string, 0, 1)
	err := commitThenRun(recordingCommitter{err: commitErr, order: &order}, func() {
		order = append(order, "notify")
	})
	if !errors.Is(err, commitErr) {
		t.Fatalf("commitThenRun error=%v want %v", err, commitErr)
	}
	if want := []string{"commit"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order=%v want %v; notification must not run after failed commit", order, want)
	}
}
