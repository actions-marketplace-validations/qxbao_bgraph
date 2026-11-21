package runner

import (
	"reflect"
	"testing"

	"github.com/qxbao/bgraph/internal/model"
)

// Đơn vị Whole (không biết tập tệp test) → lệnh cả dự án, không kèm danh sách; không bao giờ là danh sách rỗng.
func TestWholeUnit(t *testing.T) {
	s := model.Selection{Scope: model.TierGraph, Tests: []model.SelectedTest{
		{Unit: "<all-tests:.>", Project: ".", Runner: "jest", Whole: true},
		{Unit: "src/a.test.js", Project: ".", Runner: "jest", Files: []string{"src/a.test.js"}},
	}}
	p, err := Commands(s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Commands) != 1 || !reflect.DeepEqual(p.Commands[0].Argv, []string{"npx", "jest"}) {
		t.Fatalf("%+v", p.Commands)
	}
}
