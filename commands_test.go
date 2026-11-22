package bgraph

import (
	"slices"
	"strings"
	"testing"
)

func TestCommandsAndExplain(t *testing.T) {
	sel := Selection{Scope: TierGraph, Tests: []SelectedTest{{Unit: "src/a.test.ts", Project: ".", Runner: "vitest", Files: []string{"src/a.test.ts"}}}}
	plan, err := Commands(sel, CommandOptions{})
	if err != nil || plan.Scope != TierGraph || len(plan.Commands) != 1 || !slices.Equal(plan.Commands[0].Argv, []string{"npx", "vitest", "run", "src/a.test.ts"}) {
		t.Fatalf("plan %+v, err %v", plan, err)
	}
	if lines := Explain(sel, "src/a.test.ts"); len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "src/a.test.ts") {
		t.Errorf("explain: %q", lines)
	}

	full := Selection{Scope: TierFull, Widening: []Widening{{Tier: TierFull, Reason: ReasonNoBase}}}
	if plan, err := Commands(full, CommandOptions{}); err != nil || plan.Scope != TierFull || len(plan.Commands) != 0 {
		t.Errorf("full plan %+v, err %v", plan, err)
	}
}
