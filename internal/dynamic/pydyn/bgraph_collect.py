# bgraph dynamic-layer collector (docs/dynamic-layer.md section 4). Loaded with `python -m pytest -p bgraph_collect ...` (or by the
# Django bootstrap) from a temporary directory on PYTHONPATH. It installs an audit hook as early as possible and, at exit, writes
# what the run loaded, opened or listed inside the repository, and why the observation is not complete, to the JSON file named
# by BGRAPH_COLLECT_OUT. Everything bgraph cannot see (subprocesses, native code, tracing tools, its own errors) makes the
# record incomplete: an incomplete record is never used to narrow a selection.
import atexit
import os
import sys
import threading

VERSION = "python-audit-1"

_ROOT = os.path.realpath(os.environ.get("BGRAPH_COLLECT_ROOT", os.getcwd()))
_ROOTN = os.path.normcase(_ROOT)
_OUT = os.environ.get("BGRAPH_COLLECT_OUT", "")
_TRUST_NATIVE = os.environ.get("BGRAPH_COLLECT_TRUST_NATIVE", "") == "1"


import functools


@functools.lru_cache(maxsize=65536)
def _norm(p):
    return os.path.normcase(os.path.realpath(p))


def _under(n, d):
    return n == d or n.startswith(d.rstrip(os.sep) + os.sep)


# Environment directories that happen to lie inside the repository (a .venv there): covered by the environment fingerprint.
# A prefix that CONTAINS the repository is not excluded (that would exclude the repository itself).
_ENV_DIRS = []
for _p in {sys.prefix, sys.exec_prefix, sys.base_prefix, sys.base_exec_prefix}:
    try:
        _n = _norm(_p)
    except Exception:
        continue
    if _under(_n, _ROOTN) and _n != _ROOTN:
        _ENV_DIRS.append(_n)

# CPython's own directories: the standard library and its extension modules (DLLs on Windows, lib-dynload elsewhere).
_STD_DIRS = []
try:
    import sysconfig

    for _k in ("stdlib", "platstdlib"):
        _v = sysconfig.get_paths().get(_k)
        if _v:
            _STD_DIRS.append(_norm(_v))
    _v = sysconfig.get_config_var("DESTSHARED")
    if _v:
        _STD_DIRS.append(_norm(_v))
except Exception:
    pass
for _p in {sys.base_prefix, sys.base_exec_prefix}:
    _STD_DIRS.append(_norm(os.path.join(_p, "DLLs")))


def _is_site(n):
    parts = n.split(os.sep)
    return "site-packages" in parts or "dist-packages" in parts


def _cpython_own(n):
    if _is_site(n):
        return False
    return any(_under(n, d) for d in _STD_DIRS)


_lock = threading.Lock()
_local = threading.local()
_files = set()  # absolute paths opened/loaded/compiled
_dirs = set()  # absolute directories listed by non-infrastructure code
_incomplete = []
_final = [False]


def _mark(reason):
    with _lock:
        if reason not in _incomplete and len(_incomplete) < 20:
            _incomplete.append(reason)


_DEVICES = {"NUL", "CON", "PRN", "AUX", "CONIN$", "CONOUT$"} | {"COM%d" % i for i in range(1, 10)} | {"LPT%d" % i for i in range(1, 10)}


def _abs(p):
    if isinstance(p, int):
        return None
    try:
        p = os.fspath(p)
    except TypeError:
        return None
    if isinstance(p, bytes):
        p = os.fsdecode(p)
    if not p or p.startswith("<") or p == os.devnull:
        return None
    if os.name == "nt" and os.path.basename(p).split(".")[0].upper() in _DEVICES:
        return None  # a device (os.devnull is "nul"), not a file of the directory
    if not os.path.isabs(p):
        p = os.path.join(os.getcwd(), p)
    return p


def _frame_in_repo(f):
    while f is not None:
        fn = f.f_code.co_filename
        if fn and not fn.startswith("<"):
            try:
                n = _norm(fn)
            except Exception:
                n = ""
            if _under(n, _ROOTN) and not any(_under(n, d) for d in _ENV_DIRS):
                return True
        f = f.f_back
    return False


def _origin(f):
    """The first frame outside the standard library and ctypes: who is using it (for the incomplete reason)."""
    while f is not None:
        fn = f.f_code.co_filename or ""
        if not fn.startswith("<"):
            try:
                n = _norm(fn)
            except Exception:
                return fn
            if not _cpython_own(n):
                r = _rel(fn)
                if r:
                    return r
                parts = n.split(os.sep)
                for anchor in ("site-packages", "dist-packages"):
                    if anchor in parts:
                        i = len(parts) - 1 - parts[::-1].index(anchor)
                        return "/".join(parts[i + 1:])
                return os.path.basename(fn)
        f = f.f_back
    return "?"


def _repo_library(name):
    """The repository-relative path of the library a ctypes.dlopen(name) may load from the repository, or None."""
    p = _abs(name)
    if not p:
        return None
    cands = [p]
    try:
        name = os.fsdecode(os.fspath(name))
    except TypeError:
        return None
    if os.sep not in name and "/" not in name:
        # a bare name is searched in system directories, but be conservative about the current directory
        cands += [p + ".dll", p + ".so", p + ".dylib"]
    for c in cands:
        if os.path.exists(c):
            r = _rel(c)
            if r:
                return r
    return None


def _listing_counts(f):
    """A directory listing is a dependency unless the import system or pytest's own collection made it: those only decide which
    modules and test files exist, and the set of names changes only through additions, deletions and renames, which the
    selection guards already refuse to narrow (docs/dynamic-layer.md section 2)."""
    while f is not None:
        fn = f.f_code.co_filename or ""
        if fn.startswith("<frozen importlib") or fn.startswith("<frozen zipimport"):
            return False
        if fn.startswith("<"):
            f = f.f_back
            continue
        try:
            n = _norm(fn)
        except Exception:
            return True
        parts = n.split(os.sep)
        if "_pytest" in parts or "pluggy" in parts:
            return False
        if _cpython_own(n):
            f = f.f_back  # standard library helper (os.walk, glob, pathlib, pkgutil...): its caller decides
            continue
        return True
    return True


_SUBPROCESS = {
    "subprocess.Popen", "os.system", "os.exec", "os.posix_spawn", "os.spawn", "os.startfile", "os.fork", "os.forkpty",
    "_winapi.CreateProcess", "pty.spawn",
}
_CTYPES = ("ctypes.dlopen", "ctypes.dlsym", "ctypes.call_function", "ctypes.cdata", "ctypes.string_at", "ctypes.wstring_at",
           "ctypes.seh_exception")


def _hook(event, args):
    if getattr(_local, "busy", False):
        return
    _local.busy = True
    try:
        if event == "open":
            p = _abs(args[0])
            if p:
                _files.add(p)
        elif event in ("os.listdir", "os.scandir"):
            p = _abs(args[0] if args and args[0] is not None else ".")
            if p and _listing_counts(sys._getframe(1)):
                _dirs.add(p)
        elif event == "compile":
            if len(args) > 1:
                p = _abs(args[1])
                if p:
                    _files.add(p)
        elif event == "exec":
            code = args[0] if args else None
            p = _abs(getattr(code, "co_filename", None))
            if p:
                _files.add(p)
        elif event == "sqlite3.connect":
            p = _abs(args[0]) if args else None
            if p:
                _files.add(p)
        elif event in _SUBPROCESS:
            _mark("subprocess: " + event)
        elif event.startswith(_CTYPES):
            if _final[0]:
                return
            name = event.split("/")[0]
            if not _TRUST_NATIVE:
                _mark("ctypes: %s from %s" % (name, _origin(sys._getframe(1))))
            elif name == "ctypes.dlopen":
                # CPython does not audit foreign function calls (only loads and first symbol lookups), so who calls what cannot
                # be known. trust_native_env trusts the native code of the system and the environment, whoever calls it (as
                # for installed extension modules), never a library of the repository.
                lib = _repo_library(args[0] if args else None)
                if lib:
                    _mark("ctypes loads a library of the repository: " + lib)
        elif event in ("sys.settrace", "sys.setprofile", "threading.settrace", "threading.setprofile"):
            if not _final[0]:
                _mark("tracing: " + event)
    except Exception as e:  # never let the hook break the test; the record just cannot be trusted
        _mark("audit hook error: %s: %s" % (type(e).__name__, e))
    finally:
        _local.busy = False


sys.addaudithook(_hook)

# Foreign functions resolved before the hook existed can be called later without any event.
if "ctypes" in sys.modules and not _TRUST_NATIVE:
    _mark("ctypes: imported before the collector started")

# Extension modules: every one loaded from now on goes through ExtensionFileLoader.create_module.
_native = set()
try:
    import importlib.machinery as _machinery

    _orig_create = _machinery.ExtensionFileLoader.create_module

    def _create_module(self, spec):
        try:
            if spec is not None and isinstance(spec.origin, str):
                _native.add(spec.origin)
        except Exception as e:
            _mark("audit hook error: %s" % e)
        return _orig_create(self, spec)

    _machinery.ExtensionFileLoader.create_module = _create_module
except Exception as e:
    _mark("cannot watch extension modules: %s" % e)


def _ext_suffixes():
    try:
        import importlib.machinery as m

        return tuple(m.EXTENSION_SUFFIXES)
    except Exception:
        return (".so", ".pyd")


def _sweep_modules():
    sfx = _ext_suffixes()
    for m in list(sys.modules.values()):
        for p in (getattr(m, "__file__", None), getattr(getattr(m, "__spec__", None), "origin", None)):
            if isinstance(p, str) and p not in ("built-in", "frozen"):
                p = _abs(p)
                if p:
                    _files.add(p)
                    if p.endswith(sfx):
                        _native.add(p)


def _rel(p):
    try:
        n = _norm(p)
        r = os.path.realpath(p)
    except Exception:
        return None
    if n == _ROOTN or not _under(n, _ROOTN):
        return None
    if any(_under(n, d) for d in _ENV_DIRS):
        return None
    return r[len(_ROOT):].lstrip("\\/").replace(os.sep, "/")


def _check_native():
    for p in sorted(_native):
        try:
            n = _norm(p)
        except Exception:
            n = p
        if _under(n, _ROOTN) and not any(_under(n, d) for d in _ENV_DIRS):
            _mark("native extension in the repository: " + (_rel(p) or p))
        elif _cpython_own(n):
            continue
        elif not _TRUST_NATIVE:
            _mark("native extension from the environment: " + os.path.basename(p))


# pytest integration: xdist / forked workers run tests in other processes.
def pytest_configure(config):
    opt = config.option
    if getattr(opt, "numprocesses", None) or getattr(opt, "forked", False) or getattr(opt, "dist", "no") not in ("no", None):
        _mark("pytest-xdist/forked workers")
    if hasattr(config, "workerinput"):
        _mark("running inside a pytest-xdist worker")


def _write():
    if not _OUT:
        return
    _local.busy = True
    try:
        _sweep_modules()
        _check_native()
        _final[0] = True
        files = set()
        for p in list(_files):
            r = _rel(p)
            if r:
                files.add(r)
        dirs = set()
        for p in list(_dirs):
            r = _rel(p)
            if r is not None:
                dirs.add(r)
            elif _norm(p) == _ROOTN:
                dirs.add(".")
        import json

        data = {"collector": VERSION, "files": sorted(files), "dirs": sorted(dirs), "incomplete": list(_incomplete)}
        tmp = _OUT + ".tmp"
        with open(tmp, "w", encoding="utf-8") as fh:
            json.dump(data, fh)
        os.replace(tmp, _OUT)
    except Exception as e:
        sys.stderr.write("bgraph_collect: cannot write %s: %s\n" % (_OUT, e))


atexit.register(_write)


def bootstrap(argv):
    """Runs a driver script (Django's tests/runtests.py or manage.py) as __main__ with the hook installed: argv[0] is the script."""
    import runpy

    script = os.path.abspath(argv[0])
    sys.argv = [script] + list(argv[1:])
    sys.path[0] = os.path.dirname(script)
    runpy.run_path(script, run_name="__main__")
