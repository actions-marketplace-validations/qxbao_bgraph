package evalkit

import (
	"strings"
	"testing"
)

func TestPHPJUnitVerdict(t *testing.T) {
	report := `<testsuites><testsuite name="all"><testsuite><testcase name="healthy"/><testcase name="broken"><failure>assertion</failure></testcase><testcase name="exception"><error>runtime</error></testcase><testcase name="skipped"><skipped/></testcase></testsuite></testsuite></testsuites>`
	tests, red, err := phpJUnitVerdict(strings.NewReader(report))
	if err != nil || tests != 4 || !red {
		t.Fatalf("tests=%d red=%t err=%v", tests, red, err)
	}
	tests, red, err = phpJUnitVerdict(strings.NewReader(`<testsuite><testcase name="a"/></testsuite>`))
	if err != nil || tests != 1 || red {
		t.Fatalf("tests=%d red=%t err=%v", tests, red, err)
	}
	if _, _, err := phpJUnitVerdict(strings.NewReader(`<testsuite>`)); err == nil {
		t.Fatal("truncated report accepted")
	}
}
