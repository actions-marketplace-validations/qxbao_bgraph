<?php
// bgraph's PHP collector (docs/dynamic-layer.md section 11.4). `bgraph record --eco php` runs every test file of a Composer
// project in a process tree of its own with an ini file on PHP_INI_SCAN_DIR that makes this file the auto_prepend_file, so it
// runs first in every PHP process of the tree (children inherit the environment). It starts Xdebug's line coverage and a
// function monitor before anything else, replaces the file:// stream wrapper by a proxy that logs every repository path
// opened, stat'ed or listed, and at shutdown (as the last shutdown function) writes $BGRAPH_PHP_OUT/<pid>-<random>.json.
// bgraph merges the files of a test file (internal/dynamic/phpdyn).
//
// It must not load any class through Composer: everything it needs is defined here.

/** @internal bgraph collector state. */
final class BgraphPhpCollect
{
    public static string $self = '';
    /** @var list<string> */
    public static array $roots = [];
    /** @var list<string> */
    public static array $vendors = [];
    /** @var list<string> */
    public static array $runners = [];
    public static bool $win = false;
    public static bool $covering = false;
    public static bool $proxy = false;
    /** @var list<string> */
    public static array $incomplete = [];

    public static function incomplete(string $why): void
    {
        if (count(self::$incomplete) < 50 && !in_array($why, self::$incomplete, true)) {
            self::$incomplete[] = $why;
        }
    }

    /** @param list<int> $lines sorted */
    public static function ranges(array $lines): string
    {
        $o = [];
        $n = count($lines);
        for ($i = 0; $i < $n; $i++) {
            $a = $lines[$i];
            while ($i + 1 < $n && $lines[$i + 1] === $lines[$i] + 1) {
                $i++;
            }
            $o[] = $a === $lines[$i] ? (string) $a : $a . '-' . $lines[$i];
        }
        return implode(',', $o);
    }
}

/**
 * @internal A file:// stream wrapper that logs every path it is asked to open, stat or list, then does the operation with the
 * built-in wrapper (restored for the duration of the call).
 */
final class BgraphPhpFileProxy
{
    /** @var array<string, array{k: list<string>, pre: bool, w: bool}> */
    public static array $seen = [];
    private static int $probes = 0;
    /** @var resource|null */
    public $context;
    /** @var resource|false */
    private $h = false;
    /** @var resource|false */
    private $d = false;

    // The caller of the file operation: the first frame with a file that is not this collector.
    private static function caller(): string
    {
        foreach (debug_backtrace(DEBUG_BACKTRACE_IGNORE_ARGS, 6) as $f) {
            if (isset($f['file']) && $f['file'] !== BgraphPhpCollect::$self) {
                return str_replace('\\', '/', (string) $f['file']);
            }
        }
        return '';
    }

    // exempt: stats and listings made by the runner's own package or Composer's class loader decide what exists, which only
    // additions, deletions and renames change (refused by the guards).
    private static function exempt(): bool
    {
        $c = self::caller();
        if ($c === '') {
            return false;
        }
        $k = BgraphPhpCollect::$win ? strtolower($c) : $c;
        foreach (BgraphPhpCollect::$runners as $r) {
            if (str_starts_with($k, $r)) {
                return true;
            }
        }
        $loader = BgraphPhpCollect::$win ? 'composer/classloader.php' : 'composer/ClassLoader.php';
        foreach (BgraphPhpCollect::$vendors as $v) {
            if ($k === $v . $loader) {
                return true;
            }
        }
        return false;
    }

    private static function abs(string $p): string
    {
        if (str_starts_with($p, 'file://')) {
            $p = substr($p, 7);
        }
        $p = str_replace('\\', '/', $p);
        $isAbs = str_starts_with($p, '/') || preg_match('#^[A-Za-z]:/#', $p) === 1;
        if (!$isAbs) {
            $p = str_replace('\\', '/', (string) getcwd()) . '/' . $p;
        }
        $pre = '';
        if (preg_match('#^([A-Za-z]:)(/.*)$#', $p, $m) === 1) {
            [$pre, $p] = [$m[1], $m[2]];
        }
        $parts = [];
        foreach (explode('/', $p) as $s) {
            if ($s === '' || $s === '.') {
                continue;
            }
            if ($s === '..') {
                array_pop($parts);
                continue;
            }
            $parts[] = $s;
        }
        return $pre . '/' . implode('/', $parts);
    }

    // log records an event on path p: kind is include, read, write, stat or list; ok tells whether the path existed.
    private static function log(string $p, string $kind, bool $ok, bool $file = false): void
    {
        if ($p === '' || str_contains($p, "\0")) {
            return;
        }
        $a = self::abs($p);
        if (!isset(self::$seen[$a])) {
            self::$seen[$a] = ['k' => [], 'pre' => false, 'w' => false, 'wok' => false, 'wf' => false];
        }
        $e = &self::$seen[$a];
        if (!in_array($kind, $e['k'], true)) {
            $e['k'][] = $kind;
        }
        if ($kind === 'write') {
            $e['w'] = true;
            $e['wok'] = $e['wok'] || $ok; // for a write, ok tells whether it succeeded
            $e['wf'] = $e['wf'] || $ok && $file; // a file's content was written (not a directory created or a file removed)
        } elseif ($ok && !$e['w']) {
            $e['pre'] = true; // it existed before this process wrote it
        }
    }

    /** @return mixed */
    private static function call(callable $f)
    {
        stream_wrapper_restore('file');
        try {
            return $f();
        } finally {
            stream_wrapper_unregister('file');
            stream_wrapper_register('file', self::class);
        }
    }

    public static function selfTest(): bool
    {
        $before = self::$probes;
        clearstatcache();
        @file_exists(BgraphPhpCollect::$self . '.bgraph-probe');
        return self::$probes > $before;
    }

    public function stream_open(string $path, string $mode, int $options, &$opened): bool
    {
        $ctx = $this->context;
        $use = (bool) ($options & STREAM_USE_PATH);
        $this->h = self::call(static fn () => $ctx ? @fopen($path, $mode, $use, $ctx) : @fopen($path, $mode, $use));
        $ok = $this->h !== false;
        if ($options & 0x80) { // STREAM_OPEN_FOR_INCLUDE
            self::log($path, 'include', $ok);
        } else {
            $w = strpbrk($mode, 'waxc+') !== false;
            if ($w) {
                if (str_contains($mode, '+') && strpbrk($mode[0], 'rac') !== false) {
                    self::log($path, 'read', $ok);
                }
                self::log($path, 'write', $ok, true);
            } else {
                self::log($path, 'read', $ok);
            }
        }
        return $ok;
    }

    public function stream_read(int $n): string|false { return fread($this->h, $n); }
    public function stream_write(string $d): int|false { return fwrite($this->h, $d); }
    public function stream_eof(): bool { return feof($this->h); }
    public function stream_tell(): int|false { return ftell($this->h); }
    public function stream_seek(int $o, int $w = SEEK_SET): bool { return fseek($this->h, $o, $w) === 0; }
    public function stream_flush(): bool { return fflush($this->h); }
    public function stream_close(): void { if ($this->h) { fclose($this->h); } }
    /** @return array<int|string, int>|false */
    public function stream_stat(): array|false { return fstat($this->h); }
    public function stream_lock(int $op): bool { return $op === 0 ? true : flock($this->h, $op); }
    public function stream_truncate(int $n): bool { return ftruncate($this->h, $n); }
    public function stream_set_option(int $o, int $a, ?int $b): bool
    {
        switch ($o) {
            case STREAM_OPTION_BLOCKING:
                return stream_set_blocking($this->h, (bool) $a);
            case STREAM_OPTION_READ_TIMEOUT:
                return stream_set_timeout($this->h, $a, (int) $b);
            case STREAM_OPTION_WRITE_BUFFER:
                return stream_set_write_buffer($this->h, (int) $b) === 0;
        }
        return false;
    }
    /** @return resource|false */
    public function stream_cast(int $as) { return $this->h; }

    public function stream_metadata(string $p, int $o, mixed $v): bool
    {
        $r = (bool) self::call(static function () use ($p, $o, $v) {
            switch ($o) {
                case STREAM_META_TOUCH:
                    return is_array($v) && $v !== [] ? @touch($p, $v[0] ?? null, $v[1] ?? null) : @touch($p);
                case STREAM_META_OWNER_NAME:
                case STREAM_META_OWNER:
                    return @chown($p, $v);
                case STREAM_META_GROUP_NAME:
                case STREAM_META_GROUP:
                    return @chgrp($p, $v);
                case STREAM_META_ACCESS:
                    return @chmod($p, $v);
            }
            return false;
        });
        self::log($p, 'write', $r);
        return $r;
    }

    /** @return array<int|string, int>|false */
    public function url_stat(string $p, int $f): array|false
    {
        if (str_contains($p, 'bgraph-probe')) {
            self::$probes++;
            return false;
        }
        $r = self::call(static fn () => ($f & STREAM_URL_STAT_LINK) ? @lstat($p) : @stat($p));
        if (!self::exempt()) {
            self::log($p, 'stat', $r !== false);
        }
        return $r;
    }

    public function dir_opendir(string $p, int $o): bool
    {
        $this->d = self::call(static fn () => @opendir($p));
        if (!self::exempt()) {
            self::log($p, 'list', $this->d !== false);
        }
        return $this->d !== false;
    }
    public function dir_readdir(): string|false { return readdir($this->d); }
    public function dir_rewinddir(): bool { rewinddir($this->d); return true; }
    public function dir_closedir(): bool { closedir($this->d); return true; }

    public function unlink(string $p): bool
    {
        $r = (bool) self::call(static fn () => @unlink($p));
        self::log($p, 'write', $r);
        return $r;
    }
    public function rename(string $a, string $b): bool
    {
        $r = (bool) self::call(static fn () => @rename($a, $b));
        self::log($a, 'write', $r);
        self::log($b, 'write', $r, true);
        return $r;
    }
    public function mkdir(string $p, int $m, int $o): bool
    {
        $r = (bool) self::call(static fn () => @mkdir($p, $m, (bool) ($o & STREAM_MKDIR_RECURSIVE)));
        self::log($p, 'write', $r);
        return $r;
    }
    public function rmdir(string $p, int $o): bool
    {
        $r = (bool) self::call(static fn () => @rmdir($p));
        self::log($p, 'write', $r);
        return $r;
    }
}

(static function (): void {
    $out = getenv('BGRAPH_PHP_OUT');
    if (!is_string($out) || $out === '' || defined('BGRAPH_PHP_COLLECT')) {
        return;
    }
    define('BGRAPH_PHP_COLLECT', 'php-xdebug-1');
    $win = DIRECTORY_SEPARATOR === '\\';
    $norm = static function (string $p) use ($win): string {
        $p = str_replace('\\', '/', $p);
        return $win ? strtolower($p) : $p;
    };
    $root = rtrim(str_replace('\\', '/', (string) getenv('BGRAPH_PHP_ROOT')), '/');
    $roots = [$norm($root) . '/'];
    $real = realpath($root);
    if ($real !== false) {
        $roots[] = $norm(rtrim($real, '/\\')) . '/';
    }
    $roots = array_values(array_unique($roots));
    $list = static function (string $env) use ($norm): array {
        $v = (string) getenv($env);
        $o = [];
        foreach ($v === '' ? [] : explode(PATH_SEPARATOR, $v) as $d) {
            $o[] = $norm(rtrim($d, '/\\')) . '/';
        }
        return $o;
    };
    $vendors = $list('BGRAPH_PHP_VENDOR');   // absolute vendor directories: never dependencies (the fingerprint covers them)
    $runners = $list('BGRAPH_PHP_RUNNER');   // the runner's own package directories
    $parent = (string) getenv('BGRAPH_PHP_PARENT');
    $token = getmypid() . '-' . bin2hex(random_bytes(6));
    // Children (PHPUnit's process isolation passes $_SERVER as the environment) name this process as their parent.
    putenv('BGRAPH_PHP_PARENT=' . $token);
    $_SERVER['BGRAPH_PHP_PARENT'] = $token;
    $_ENV['BGRAPH_PHP_PARENT'] = $token;

    BgraphPhpCollect::$self = __FILE__;
    BgraphPhpCollect::$roots = $roots;
    BgraphPhpCollect::$vendors = $vendors;
    BgraphPhpCollect::$runners = $runners;
    BgraphPhpCollect::$win = $win;
    $c = BgraphPhpCollect::class;
    if (!function_exists('xdebug_start_code_coverage') || !function_exists('xdebug_start_function_monitor')) {
        $c::incomplete('Xdebug coverage and develop modes are not active in this process');
    } else {
        if (ini_get('pcov.enabled')) {
            $c::incomplete('another coverage tool is active (pcov.enabled=1)');
        }
        if (function_exists('xdebug_code_coverage_started') && xdebug_code_coverage_started()) {
            $c::incomplete('code coverage was already started before the collector');
        }
        // Coverage of the repository without the vendor directories: Xdebug's filter takes one kind of list, so the
        // repository is described by the prefixes of its entries that are not vendor directories.
        $prefixes = [];
        $walk = static function (string $dir) use (&$walk, &$prefixes, $vendors, $norm): void {
            $ents = @scandir($dir);
            if ($ents === false) {
                $prefixes[] = $dir . '/';
                return;
            }
            foreach ($ents as $e) {
                if ($e === '.' || $e === '..') {
                    continue;
                }
                $p = $dir . '/' . $e;
                $k = $norm($p) . '/';
                if (in_array($k, $vendors, true)) {
                    continue;
                }
                $above = false;
                foreach ($vendors as $v) {
                    if (str_starts_with($v, $k)) {
                        $above = true;
                    }
                }
                if ($above && is_dir($p)) {
                    $walk($p);
                } else {
                    $prefixes[] = is_dir($p) ? $p . '/' : $p;
                }
            }
        };
        $walk($root);
        if ($real !== false && $norm(rtrim($real, '/\\')) !== $norm($root)) {
            $walk(rtrim(str_replace('\\', '/', $real), '/'));
        }
        xdebug_set_filter(XDEBUG_FILTER_CODE_COVERAGE, XDEBUG_PATH_INCLUDE, $prefixes === [] ? [$root . '/'] : $prefixes);
        xdebug_start_code_coverage(XDEBUG_CC_UNUSED | XDEBUG_CC_DEAD_CODE);
        xdebug_start_function_monitor([
            'proc_open', 'exec', 'shell_exec', 'system', 'passthru', 'popen', 'mail', 'mb_send_mail',
            'pcntl_fork', 'pcntl_rfork', 'pcntl_forkx', 'pcntl_exec', 'dl', 'FFI::cdef', 'FFI::load', 'FFI::scope',
            'xdebug_start_code_coverage', 'xdebug_stop_code_coverage',
            'pcov\start', 'pcov\stop', 'pcov\clear',
        ]);
        $c::$covering = true;
    }
    if (!@stream_wrapper_unregister('file') || !stream_wrapper_register('file', BgraphPhpFileProxy::class)) {
        $c::incomplete('cannot install the file:// stream wrapper proxy');
    } else {
        $c::$proxy = true;
    }
    $t0 = microtime(true);
    $write = static function () use ($c, $out, $token, $parent, $roots, $vendors, $runners, $norm, $t0): void {
        $proxied = $c::$proxy && BgraphPhpFileProxy::selfTest();
        if ($c::$proxy) {
            @stream_wrapper_restore('file');
        }
        if ($c::$proxy && !$proxied) {
            $c::incomplete('the file:// stream wrapper proxy was replaced or removed during the run');
        }
        $inRepo = static function (string $p) use ($roots, $vendors, $norm): bool {
            $k = $norm($p);
            foreach ($vendors as $v) {
                if (str_starts_with($k, $v)) {
                    return false;
                }
            }
            foreach ($roots as $r) {
                if (str_starts_with($k, $r)) {
                    return true;
                }
            }
            return false;
        };
        $cov = [];
        $mon = [];
        if ($c::$covering) {
            foreach (xdebug_get_code_coverage() as $file => $lines) {
                if (!$inRepo((string) $file)) {
                    continue;
                }
                ksort($lines);
                $exec = [];
                $none = [];
                foreach ($lines as $n => $s) {
                    if ($s > 0) {
                        $exec[] = (int) $n;
                    } elseif ($s === -1) {
                        $none[] = (int) $n;
                    }
                }
                $cov[(string) $file] = BgraphPhpCollect::ranges($exec) . '|' . BgraphPhpCollect::ranges($none);
            }
            foreach (xdebug_get_monitored_functions() as $m) {
                $f = (string) ($m['filename'] ?? '');
                if ($f === BgraphPhpCollect::$self) {
                    continue;
                }
                $fn = strtolower((string) ($m['function'] ?? ''));
                $where = str_replace('\\', '/', $f) . ':' . (int) ($m['lineno'] ?? 0);
                switch ($fn) {
                    case 'xdebug_start_code_coverage':
                    case 'xdebug_stop_code_coverage':
                    case 'pcov\\start':
                    case 'pcov\\stop':
                    case 'pcov\\clear':
                        $c::incomplete('another coverage user: ' . $fn . ' at ' . $where);
                        break;
                    case 'pcntl_fork':
                    case 'pcntl_rfork':
                    case 'pcntl_forkx':
                        $c::incomplete('forks the process (' . $fn . ') at ' . $where);
                        break;
                    case 'pcntl_exec':
                        $c::incomplete('replaces the process (pcntl_exec) at ' . $where);
                        break;
                    case 'dl':
                        $c::incomplete('loads a PHP extension (dl) at ' . $where);
                        break;
                    case 'ffi::cdef':
                    case 'ffi::load':
                    case 'ffi::scope':
                        $c::incomplete('calls native code through FFI at ' . $where);
                        break;
                    default:
                        $runner = false;
                        foreach ($runners as $r) {
                            if (str_starts_with($norm($f), $r)) {
                                $runner = true;
                            }
                        }
                        if ($runner) {
                            $mon[] = ['fn' => $fn, 'at' => $where];
                        } else {
                            $c::incomplete('starts a process (' . $fn . ') at ' . $where);
                        }
                }
            }
        }
        $events = [];
        $outside = [];
        $own = $norm(rtrim($out, '/\\')) . '/'; // the output directory holds the process tree's own TMPDIR and caches
        foreach (BgraphPhpFileProxy::$seen as $p => $e) {
            if ($inRepo($p)) {
                $events[$p] = $e;
                continue;
            }
            // Outside the repository (or in a vendor directory) and outside the test file's own temporary directory: a file
            // written there is state another test file, or a later run, may see without reading any repository file; bgraph
            // checks the files read or included there against what other test files wrote and against composer install.
            $k = $norm($p);
            if (str_starts_with($k, $own) || str_starts_with($k, '/dev/')) {
                continue;
            }
            $wf = $e['wf'] ?? false;
            $r = in_array('read', $e['k'], true) || in_array('include', $e['k'], true);
            if ($wf || $r) {
                $outside[] = ['p' => $p, 'w' => $wf, 'r' => $r];
            }
        }
        foreach (get_included_files() as $f) {
            if ($f !== BgraphPhpCollect::$self && !$inRepo($f) && !isset(BgraphPhpFileProxy::$seen[$f])) {
                $k = $norm($f);
                if (!str_starts_with($k, $own)) {
                    $outside[] = ['p' => $f, 'w' => false, 'r' => true];
                }
            }
        }
        $included = [];
        foreach (get_included_files() as $f) {
            if ($f !== BgraphPhpCollect::$self && $inRepo($f)) {
                $included[] = $f;
            }
        }
        $data = [
            'collector' => 'php-xdebug-1',
            'pid' => getmypid(),
            'ppid' => function_exists('posix_getppid') ? posix_getppid() : 0,
            'token' => $token,
            'parent' => $parent,
            'included' => $included,
            'coverage' => $cov,
            'events' => $events,
            'outside' => $outside,
            'launches' => $mon,
            'incomplete' => $c::$incomplete,
            'secs' => microtime(true) - $t0,
        ];
        $json = json_encode($data, JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE);
        @file_put_contents($out . '/' . getmypid() . '-' . bin2hex(random_bytes(6)) . '.json', $json === false ? '{}' : $json);
    };
    // Shutdown functions run in registration order: registering again from inside one puts the writer last.
    register_shutdown_function(static function () use ($write): void {
        register_shutdown_function($write);
    });
})();
