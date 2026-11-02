package jobs

import (
	"testing"

	"github.com/shubham-kukreja/core-jobs-service/internal/jobs/internal/db"
)

func ptr[T any](v T) *T { return &v }

func dbRun(errJSON string) db.JobsJobRun {
	return db.JobsJobRun{ID: 1, Kind: "model.run", Args: []byte(`{}`), Status: "failed", Error: []byte(errJSON)}
}

func TestBuildTree(t *testing.T) {
	runs := []JobRun{
		{ID: 1, Kind: "import.file"},
		{ID: 2, Kind: "derive.cascade", ParentID: ptr(int64(1))},
		{ID: 3, Kind: "quality.check", ParentID: ptr(int64(1))},
		{ID: 4, Kind: "veto.evaluate", ParentID: ptr(int64(2))},
	}
	tree := buildTree(1, runs)
	if tree.ID != 1 || len(tree.Children) != 2 {
		t.Fatalf("root: %+v", tree)
	}
	if tree.Children[0].ID != 2 || tree.Children[1].ID != 3 {
		t.Fatalf("sibling order: %d %d", tree.Children[0].ID, tree.Children[1].ID)
	}
	if len(tree.Children[0].Children) != 1 || tree.Children[0].Children[0].Kind != "veto.evaluate" {
		t.Fatalf("grandchild: %+v", tree.Children[0])
	}
}

func TestDiffUpdates(t *testing.T) {
	prev := JobRun{Status: StatusRunning, Progress: 10}
	if got := diffUpdates(prev, prev); len(got) != 0 {
		t.Fatalf("no change emitted %v", got)
	}
	got := diffUpdates(prev, JobRun{Status: StatusSucceeded, Progress: 100})
	if len(got) != 2 || got[0].Kind != "progress" || got[1].Kind != "status" || got[1].Status != StatusSucceeded {
		t.Fatalf("updates = %+v", got)
	}
	if !terminal(StatusCancelled) || terminal(StatusRunning) {
		t.Fatal("terminal misclassifies")
	}
}

func TestToJobRunDecodesError(t *testing.T) {
	jr, err := toJobRun(dbRun(`{"code":"timeout","message":"job timed out","retryable":false}`))
	if err != nil || jr.Error == nil || jr.Error.Code != "timeout" {
		t.Fatalf("%+v %v", jr.Error, err)
	}
}
