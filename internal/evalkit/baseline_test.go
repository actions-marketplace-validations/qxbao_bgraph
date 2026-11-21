package evalkit

import "testing"

func TestScoreTool(t *testing.T) {
	bt := BTask{ID: "x", Repo: "r", Required: []string{"test/a.test.js"}, Units: []BUnit{
		{ID: "test/a.test.js", Project: ".", Files: []string{"test/a.test.js"}},
		{ID: "test/b.test.js", Project: ".", Files: []string{"test/b.test.js"}},
	}}
	ok := ScoreTool(bt, "t", &ToolRun{Status: "ok", Closure: []string{"src/x.js", "test/a.test.js"}})
	if ok.Status != "ok" || ok.Selected != 1 || ok.Units != 2 {
		t.Fatalf("ok: %+v", ok)
	}
	vio := ScoreTool(bt, "t", &ToolRun{Status: "ok", Closure: []string{"src/x.js", "test/b.test.js"}})
	if vio.Status != "violation" || len(vio.Missing) != 1 {
		t.Fatalf("violation: %+v", vio)
	}
	if got := ScoreTool(bt, "t", &ToolRun{Status: "timeout"}); got.Status != "timeout" {
		t.Fatalf("timeout: %+v", got)
	}
	if got := ScoreTool(bt, "t", nil); got.Status != "missing" {
		t.Fatalf("missing: %+v", got)
	}
	wb := BTask{Required: []string{"p/t.js"}, Units: []BUnit{{ID: "p", Project: "p", Whole: true}}}
	if got := ScoreTool(wb, "t", &ToolRun{Status: "ok", Closure: []string{"p/src.js"}}); got.Status != "ok" || got.Whole != 1 {
		t.Fatalf("whole: %+v", got)
	}
}
