#!/usr/bin/env bash
set -euo pipefail

binary=$(realpath "${1:?Pass the path to the bgraph binary}")
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
repo="$work/repo"
mkdir -p "$repo"
cd "$repo"

mkdir -p Sources/Core Tests/AlphaTests Tests/BetaTests
cat > Package.swift <<'EOF'
// swift-tools-version: 6.0
import PackageDescription

let package = Package(name: "RunnerSmoke", targets: [
    .target(name: "Core"),
    .testTarget(name: "AlphaTests", dependencies: ["Core"]),
    .testTarget(name: "BetaTests"),
])
EOF
cat > Sources/Core/Core.swift <<'EOF'
import Testing

public let value = 42

@Test func linkedLibraryTest() {
    #expect(value == 42, "linked library test failed")
}
EOF
cat > Tests/AlphaTests/AlphaTests.swift <<'EOF'
import Core
import Testing
import XCTest

final class AlphaCase: XCTestCase {
    func testSelectedXCTest() {
        XCTAssertEqual(value, 42, "selected XCTest failed")
    }
}

@Test func selectedSwiftTesting() {
    #expect(value == 42, "selected Swift Testing failed")
}
EOF
cat > Tests/BetaTests/BetaTests.swift <<'EOF'
import Testing
import XCTest

final class BetaCase: XCTestCase {
    func testIndependentXCTest() { XCTAssertTrue(true) }
}

@Test func independentSwiftTesting() { #expect(Bool(true)) }
EOF
printf '.build/\n' > .gitignore
git init -q
git add -- .gitignore Package.swift Sources Tests
git -c user.name='bgraph CI' -c user.email='ci@example.invalid' commit -qm baseline
printf '\n// Exercise the command at a changed library target.\n' >> Sources/Core/Core.swift
"$binary" command --base HEAD > "$work/plan.sh"
mv "$work/plan.sh" plan.sh
grep -Fxq '# scope: graph' plan.sh
grep -Fxq "swift test --skip '^(?:BetaTests)\\.'" plan.sh

cat > "$repo/check.sh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

if ! timeout --kill-after=5 120 swift test > baseline.log 2>&1; then
    cat baseline.log
    exit 1
fi
grep -Fq 'testIndependentXCTest' baseline.log
grep -Fq 'independentSwiftTesting()' baseline.log
sed 's/public let value = 42/public let value = 41/' Sources/Core/Core.swift > Core.swift
mv Core.swift Sources/Core/Core.swift
status=0
timeout --kill-after=5 120 bash plan.sh > selected.log 2>&1 || status=$?
cat selected.log
if [ "$status" -ne 1 ]; then
    echo "Expected a test failure, got exit status $status"
    exit 1
fi
grep -Fq 'selected XCTest failed' selected.log
grep -Fq 'selected Swift Testing failed' selected.log
grep -Fq 'linked library test failed' selected.log
if grep -Eq 'testIndependentXCTest|independentSwiftTesting\(\)' selected.log; then
    echo 'The generated command ran an independent test'
    exit 1
fi
EOF

image=${BGRAPH_SWIFT_IMAGE:-swift@sha256:0e4716bd34384d22963a63afbdbc93be3129dfd0753185aa1ded27755abdcae8}
docker run --rm --init --user "$(id -u):$(id -g)" --env HOME=/fixture \
    --volume "$repo:/fixture" --workdir /fixture "$image" bash check.sh
