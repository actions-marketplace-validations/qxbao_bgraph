package phpdyn

import (
	"strings"
	"testing"
)

const loggerPHP = `<?php
declare(strict_types=1);

namespace App;

use function strlen;
use Psr\Log\LoggerInterface;

/**
 * A logger.
 */
final class Logger implements LoggerInterface
{
    public const LEVEL = 3;
    private int $count = 0;

    /** @return int */
    public function count(): int
    {
        return $this->count;
    }

    #[\Deprecated]
    public function log(string $m): void
    {
        $f = function () use ($m) {
            return strlen($m);
        };
        $this->count += $f();
    }

    abstract protected function hidden(): void;
}

function helper(int $x): int
{
    return $x + 1;
}
`

func guard(t *testing.T, base, head string) ([]Span, bool, int, string) {
	t.Helper()
	return BodyGuard("x.php", []byte(base), []byte(head))
}

func TestBodyGuardAcceptsBodyChanges(t *testing.T) {
	head := strings.Replace(loggerPHP, "return $this->count;", "return $this->count + 0;", 1)
	spans, shift, _, why := guard(t, loggerPHP, head)
	if why != "" || len(spans) != 1 || spans[0].From != 18 || spans[0].To != 21 || spans[0].What != `App\Logger::count` || shift {
		t.Fatalf("spans %+v shift %v why %q", spans, shift, why)
	}
	// A closure inside a named body is part of that body; two functions change.
	head = strings.Replace(head, "return strlen($m);", "return strlen($m) * 2;", 1)
	head = strings.Replace(head, "return $x + 1;", "return $x + 2;", 1)
	spans, _, _, why = guard(t, loggerPHP, head)
	if why != "" || len(spans) != 3 {
		t.Fatalf("spans %+v why %q", spans, why)
	}
}

func TestBodyGuardCommentsAndShift(t *testing.T) {
	// Plain comments and blank lines anywhere are free; a line count change is reported with the first changed line.
	head := strings.Replace(loggerPHP, "use Psr\\Log\\LoggerInterface;\n", "use Psr\\Log\\LoggerInterface;\n// note\n\n", 1)
	spans, shift, first, why := guard(t, loggerPHP, head)
	if why != "" || len(spans) != 0 || !shift || first != 8 {
		t.Fatalf("spans %+v shift %v first %d why %q", spans, shift, first, why)
	}
	head = strings.Replace(loggerPHP, "        return $this->count;\n", "        // why\n        return $this->count;\n", 1)
	spans, shift, first, why = guard(t, loggerPHP, head)
	if why != "" || len(spans) != 0 || !shift || first != 20 {
		t.Fatalf("comment in a body: spans %+v shift %v first %d why %q", spans, shift, first, why)
	}
}

func TestBodyGuardRefusals(t *testing.T) {
	cases := map[string][2]string{
		"signature":           {"public function count(): int", "public function count(): ?int"},
		"doc comment":         {"/** @return int */", "/** @return positive-int */"},
		"class doc comment":   {" * A logger.", " * The logger."},
		"attribute":           {`#[\Deprecated]`, `#[\Deprecated("x")]`},
		"constant":            {"public const LEVEL = 3;", "public const LEVEL = 4;"},
		"property default":    {"private int $count = 0;", "private int $count = 1;"},
		"use statement":       {`use Psr\Log\LoggerInterface;`, `use Psr\Log\LoggerInterface as L;`},
		"declare":             {"declare(strict_types=1);", "declare(strict_types=0);"},
		"namespace":           {"namespace App;", "namespace App2;"},
		"one-line function":   {"function helper(int $x): int\n{\n    return $x + 1;\n}", "function helper(int $x): int { return $x + 2; }"},
		"unparsable head":     {"return $x + 1;", "return $x + 1; /* open"},
		"short open tag head": {"<?php\ndeclare", "<?\ndeclare"},
	}
	for name, c := range cases {
		head := strings.Replace(loggerPHP, c[0], c[1], 1)
		if head == loggerPHP {
			t.Fatalf("%s: edit did not apply", name)
		}
		if _, _, _, why := guard(t, loggerPHP, head); why == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBodyGuardDeclarationsInBodies(t *testing.T) {
	nested := `<?php
class A
{
    public function f()
    {
        function g() { return 1; }
        return 2;
    }
}
`
	head := strings.Replace(nested, "return 2;", "return 3;", 1)
	if _, _, _, why := guard(t, nested, head); !strings.Contains(why, "declares the function g") {
		t.Fatalf("nested function: %q", why)
	}
	class := strings.Replace(nested, "function g() { return 1; }", "class Inner {}", 1)
	head = strings.Replace(class, "return 2;", "return 3;", 1)
	if _, _, _, why := guard(t, class, head); !strings.Contains(why, "declares the class Inner") {
		t.Fatalf("class in a body: %q", why)
	}
	// An anonymous class inside a named body is part of the body.
	anon := strings.Replace(nested, "function g() { return 1; }", "$o = new class { public function h() { return 1; } };", 1)
	head = strings.Replace(anon, "return 1; } };", "return 5; } };", 1)
	if spans, _, _, why := guard(t, anon, head); why != "" || len(spans) != 1 || spans[0].What != "A::f" {
		t.Fatalf("anonymous class in a body: %+v %q", spans, why)
	}
}

func TestBodyGuardCodeOutsideNamedBodies(t *testing.T) {
	cases := map[string]string{
		"closure at file level":         "<?php\n$f = function () {\n    return 1;\n};\n",
		"anonymous class at file level": "<?php\n$o = new class {\n    public function h()\n    {\n        return 1;\n    }\n};\n",
		"conditional function":          "<?php\nif (!function_exists('h')) {\n    function h()\n    {\n        return 1;\n    }\n}\n",
		"conditional class":             "<?php\nif (true) {\n    class C\n    {\n        public function h()\n        {\n            return 1;\n        }\n    }\n}\n",
		"alternative syntax":            "<?php\nif (true):\n    function h()\n    {\n        return 1;\n    }\nendif;\n",
	}
	for name, src := range cases {
		head := strings.Replace(src, "return 1;", "return 2;", 1)
		if _, _, _, why := guard(t, src, head); why == "" {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBodyGuardEnumAndInterface(t *testing.T) {
	src := `<?php
namespace N {
    enum Level: int
    {
        case A = 1;

        public function label(): string
        {
            return match ($this) {
                self::A => 'a',
            };
        }
    }

    interface I
    {
        public function x(): void;
    }
}
`
	head := strings.Replace(src, "self::A => 'a',", "self::A => 'b',", 1)
	spans, _, _, why := guard(t, src, head)
	if why != "" || len(spans) != 1 || spans[0].What != `N\Level::label` || spans[0].From != 7 || spans[0].To != 12 {
		t.Fatalf("%+v %q", spans, why)
	}
	head = strings.Replace(src, "case A = 1;", "case A = 2;", 1)
	if _, _, _, why := guard(t, src, head); why == "" {
		t.Fatal("enum case change accepted")
	}
}

func TestSpans(t *testing.T) {
	got, ok := Spans([]byte(loggerPHP))
	if !ok {
		t.Fatal("not parsed")
	}
	want := []FuncSpan{{18, 21}, {24, 30}, {35, 38}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}
