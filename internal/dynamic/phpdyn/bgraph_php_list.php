<?php
// bgraph's PHP test-file listing (docs/dynamic-layer.md section 11.3). Arguments: the project's Composer autoloader and its
// PHPUnit XML configuration file. It maps the configured test suites to files the way PHPUnit's own TestSuiteMapper does
// (the default test suite, directories with prefix, suffix and phpVersion, files, excludes), with PHPUnit's configuration
// loader and sebastian/file-iterator, without loading any test file (data providers, which --list-test-files runs, may need
// extensions or services the listing does not). Pest uses the same configuration and discovery. Output: one JSON object.
error_reporting(E_ALL & ~E_DEPRECATED & ~E_USER_DEPRECATED);
$res = ['files' => [], 'phpunit' => '', 'pest' => '', 'error' => ''];
try {
    require $argv[1];
    if (class_exists(\Composer\InstalledVersions::class)) {
        foreach (['phpunit/phpunit' => 'phpunit', 'pestphp/pest' => 'pest'] as $pkg => $k) {
            if (\Composer\InstalledVersions::isInstalled($pkg)) {
                $res[$k] = (string) \Composer\InstalledVersions::getPrettyVersion($pkg);
            }
        }
    }
    if ($res['phpunit'] === '' && class_exists(\PHPUnit\Runner\Version::class)) {
        $res['phpunit'] = \PHPUnit\Runner\Version::id();
    }
    $xml = (new \PHPUnit\TextUI\XmlConfiguration\Loader())->load($argv[2]);
    $include = [];
    if ($xml->phpunit()->hasDefaultTestSuite()) {
        $include = explode(',', $xml->phpunit()->defaultTestSuite());
    }
    $seen = [];
    foreach ($xml->testSuite() as $suite) {
        if ($include !== [] && !in_array($suite->name(), $include, true)) {
            continue;
        }
        $exclude = [];
        foreach ($suite->exclude()->asArray() as $f) {
            $exclude[] = $f->path();
        }
        foreach ($suite->directories() as $d) {
            if (!str_contains($d->path(), '*') && !is_dir($d->path())) {
                throw new RuntimeException('test directory not found: ' . $d->path());
            }
            if (!version_compare(PHP_VERSION, $d->phpVersion(), $d->phpVersionOperator()->asString())) {
                continue;
            }
            $files = (new \SebastianBergmann\FileIterator\Facade())->getFilesAsArray($d->path(), $d->suffix(), $d->prefix(), $exclude);
            foreach ($files as $f) {
                $seen[$f] = true;
            }
        }
        foreach ($suite->files() as $f) {
            if (!is_file($f->path())) {
                throw new RuntimeException('test file not found: ' . $f->path());
            }
            if (!version_compare(PHP_VERSION, $f->phpVersion(), $f->phpVersionOperator()->asString())) {
                continue;
            }
            $seen[$f->path()] = true;
        }
    }
    $res['files'] = array_keys($seen);
} catch (\Throwable $e) {
    $res['error'] = get_class($e) . ': ' . $e->getMessage();
}
echo json_encode($res, JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE);
