#!/usr/bin/env bash
set -euo pipefail

binary=$(realpath "${1:?Pass the path to the bgraph binary}")
work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT
mkdir -p "$work/repo"/{cmake,meson}
cd "$work/repo"

cat > cmake/CMakeLists.txt <<'EOF'
cmake_minimum_required(VERSION 3.20)
project(cmake_smoke LANGUAGES C)
enable_testing()
add_library(core STATIC core.c)
add_executable(core_test core_test.c)
target_link_libraries(core_test PRIVATE core)
add_test(NAME core.check COMMAND core_test)
add_executable(independent_test independent.c)
add_test(NAME independent.check COMMAND independent_test)
EOF
cat > meson/meson.build <<'EOF'
project('meson_smoke', 'cpp')
core = static_library('core', 'core.cpp')
core_test = executable('core_test', 'core_test.cpp', link_with: core)
test('core.check', core_test)
independent_test = executable('independent_test', 'independent.cpp')
test('independent.check', independent_test)
EOF
for spec in cmake:c meson:cpp; do
    dir=${spec%:*}
    ext=${spec#*:}
    printf 'int answer(void) { return 42; }\n' > "$dir/core.$ext"
    printf 'int answer(void);\nint main(void) { return answer() == 42 ? 0 : 1; }\n' > "$dir/core_test.$ext"
    printf 'int main(void) { return 0; }\n' > "$dir/independent.$ext"
done
printf 'native-build/\n' > .gitignore
git init -q
git add -- .gitignore cmake meson
git -c user.name='bgraph CI' -c user.email='ci@example.invalid' commit -qm baseline

cmake -S cmake -B cmake/native-build -G Ninja
cmake --build cmake/native-build
ctest --test-dir cmake/native-build --output-on-failure --no-tests=error
meson setup meson/native-build meson
meson test -C meson/native-build --print-errorlogs

for spec in cmake:c meson:cpp; do
    dir=${spec%:*}
    ext=${spec#*:}
    file="$dir/core.$ext"
    printf 'int answer(void) { return 41; }\n' > "$file"
    "$binary" command --base HEAD --build-dir native-build > "$work/plan.sh"
    grep -Fxq '# scope: graph' "$work/plan.sh"
    case "$dir" in
        cmake) grep -Fq -- "-R '^(core\\.check)$'" "$work/plan.sh" ;;
        meson) grep -Fxq '(cd meson && meson test -C native-build --print-errorlogs core.check)' "$work/plan.sh" ;;
    esac

    status=0
    bash -e "$work/plan.sh" > "$work/selected.log" 2>&1 || status=$?
    cat "$work/selected.log"
    case "$dir:$status" in
        cmake:8|meson:1) ;;
        *) echo "Unexpected $dir test exit status $status"; exit 1 ;;
    esac
    grep -Fq 'core.check' "$work/selected.log"
    if grep -Fq 'independent.check' "$work/selected.log"; then
        echo "The $dir command ran the independent test"
        exit 1
    fi
    git restore -- "$file"
done
