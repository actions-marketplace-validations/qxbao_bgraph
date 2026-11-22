# bgraph: describes the Python environment for the dynamic layer's fingerprint (docs/dynamic.md section 5).
# Run as a script from a directory that holds nothing else, so that no repository module can shadow what it imports.
# Prints one "key=value" line per fact; the order does not matter.
import platform
import sys
import sysconfig


def _norm(name):
    out = []
    prev = False
    for ch in name.lower():
        if ch in "-_.":
            if not prev:
                out.append("-")
            prev = True
        else:
            out.append(ch)
            prev = False
    return "".join(out)


def main():
    lines = set()
    lines.add("version=" + sys.version)
    impl = sys.implementation
    lines.add("implementation=%s %s %s" % (impl.name, ".".join(str(x) for x in impl.version), impl.cache_tag))
    lines.add("platform=" + sysconfig.get_platform())
    lines.add("os=" + platform.system())
    lines.add("machine=" + platform.machine())
    import importlib.metadata as md

    for dist in md.distributions():
        try:
            name = dist.metadata["Name"] or ""
            version = dist.version or ""
        except Exception as e:  # a broken distribution is still part of the environment
            name, version = "?broken", repr(e)
        lines.add("dist=%s==%s" % (_norm(name), version))
    # Not part of the fingerprint: the directories distributions are installed in, whose modification times tell bgraph
    # when to probe again (an install or uninstall adds or removes a *.dist-info directory there).
    import os

    for entry in sys.path[1:]:  # sys.path[0] is this script's temporary directory
        if entry and os.path.isdir(entry):
            lines.add("site=" + os.path.realpath(entry))
    out = sys.stdout
    for line in sorted(lines):
        out.write(line.replace("\n", " ").replace("\r", " ") + "\n")
    out.flush()


main()
