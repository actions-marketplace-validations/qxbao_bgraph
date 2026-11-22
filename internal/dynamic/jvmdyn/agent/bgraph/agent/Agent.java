package bgraph.agent;

import java.io.ByteArrayInputStream;
import java.io.ByteArrayOutputStream;
import java.io.DataInputStream;
import java.io.DataOutputStream;
import java.io.IOException;
import java.io.PrintWriter;
import java.lang.instrument.ClassFileTransformer;
import java.lang.instrument.Instrumentation;
import java.lang.reflect.Method;
import java.nio.channels.FileChannel;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.nio.file.StandardOpenOption;
import java.security.ProtectionDomain;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Collections;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Map;
import java.util.Random;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.jar.JarFile;

/**
 * bgraph JVM collector (docs/dynamic.md section 9.5). Started with -javaagent:agent.jar=LOGDIR in every test JVM; writes
 * LOGDIR/STEM.log at exit (every class definition with its code source, SourceFile and SMAP files; FileChannel.map calls; a
 * cross-check of loaded classes) and LOGDIR/STEM.jfr (JFR file reads/writes, process starts, native libraries; turned into text
 * by Dump). The build tools' own JVMs are skipped.
 */
public final class Agent implements ClassFileTransformer {
    static final String[] SKIP_MAIN = {
        "org.codehaus.plexus.classworlds.launcher.Launcher",
        "org.gradle.launcher.daemon.bootstrap.GradleDaemon",
        "org.gradle.wrapper.GradleWrapperMain",
        "org.gradle.launcher.GradleMain",
        "org.jetbrains.kotlin.daemon.KotlinCompileDaemon",
    };
    static final String CHANNEL = "sun/nio/ch/FileChannelImpl";

    static final Map<String, Boolean> seen = new ConcurrentHashMap<>();
    static final List<String> classes = Collections.synchronizedList(new ArrayList<>());
    static volatile boolean rewriteChannel; // transform FileChannelImpl when it is retransformed
    static volatile boolean channelRewritten;
    static volatile String rewriteError;
    static volatile String hookLost; // a redefinition after installation could not keep the hook
    static Instrumentation inst;
    static String jfrStatus = "jfr\tfail\tnot started";
    static String hookStatus = "hook\tfail\tnot installed";
    static Method drain;

    public static void premain(String args, Instrumentation in) {
        try {
            start(args, in);
        } catch (Throwable t) {
            // never break the test JVM
        }
    }

    static void start(String args, Instrumentation in) throws Exception {
        String cmd = System.getProperty("sun.java.command", "");
        String main = cmd.trim().split("\\s+", 2)[0];
        for (String s : SKIP_MAIN) {
            if (s.equals(main)) {
                return;
            }
        }
        inst = in;
        Path dir = Paths.get(args == null ? "." : args);
        Files.createDirectories(dir);
        long pid = ProcessHandle.current().pid();
        String stem = pid + "-" + Long.toHexString(System.nanoTime()) + "-" + Long.toHexString(new Random().nextLong());
        String startLine = "start\t" + pid + "\t" + clean(System.getProperty("java.home")) + "\t" + clean(System.getProperty("java.version")) + "\t" + clean(cmd);
        in.addTransformer(new Agent(), true);
        Runtime.getRuntime().addShutdownHook(new Thread(() -> finish(dir.resolve(stem + ".log"), startLine)));
        // JFR first: enabling its file events redefines FileChannelImpl, which must happen before the hook is installed (the
        // transformer keeps re-inserting the hook on every later redefinition).
        try {
            jdk.jfr.Recording r = new jdk.jfr.Recording();
            r.enable("jdk.FileRead").withThreshold(Duration.ZERO).withStackTrace();
            r.enable("jdk.FileWrite").withThreshold(Duration.ZERO);
            r.enable("jdk.ProcessStart").withStackTrace();
            r.enable("jdk.NativeLibrary").with("period", "endChunk");
            r.setToDisk(true);
            r.setDestination(dir.resolve(stem + ".jfr"));
            r.setDumpOnExit(true);
            r.start();
            jfrStatus = "jfr\tok";
        } catch (Throwable t) {
            jfrStatus = "jfr\tfail\t" + clean(String.valueOf(t));
        }
        installHook(in);
    }

    /** Installs the FileChannelImpl.map hook (docs/dynamic.md section 9.5); any failure leaves hookStatus as a failure. */
    static void installHook(Instrumentation in) {
        try {
            Path jar = Paths.get(Agent.class.getProtectionDomain().getCodeSource().getLocation().toURI());
            in.appendToBootstrapClassLoaderSearch(new JarFile(jar.toFile()));
            Class<?> hook = Class.forName("bgraph.agent.Hook", true, null);
            if (hook.getClassLoader() != null) {
                throw new IllegalStateException("Hook not loaded by the bootstrap loader");
            }
            drain = hook.getMethod("drain");
            Module base = Object.class.getModule();
            in.redefineModule(base, Set.of(hook.getModule()), Map.of(), Map.of(), Set.of(), Map.of());
            Class<?> ch = Class.forName(CHANNEL.replace('/', '.'), false, null);
            java.lang.reflect.Field f = ch.getDeclaredField("path");
            if (f.getType() != String.class) {
                throw new IllegalStateException("FileChannelImpl.path is not a String");
            }
            if (!in.isModifiableClass(ch)) {
                throw new IllegalStateException("FileChannelImpl is not modifiable");
            }
            rewriteChannel = true;
            try {
                in.retransformClasses(ch);
            } catch (Throwable t) {
                rewriteChannel = false;
                throw t;
            }
            if (!channelRewritten) {
                rewriteChannel = false;
                throw new IllegalStateException("FileChannelImpl not rewritten: " + rewriteError);
            }
            // Self-test: map a few bytes of the agent jar and expect the hook to see it.
            boolean ok = false;
            try (FileChannel c = FileChannel.open(jar, StandardOpenOption.READ)) {
                c.map(FileChannel.MapMode.READ_ONLY, 0, Math.min(16, c.size()));
            }
            for (String e : (String[]) drain.invoke(null)) {
                ok = ok || e.startsWith(jar.toString() + "\t");
            }
            if (!ok) {
                throw new IllegalStateException("self-test: the hook did not see a mapping");
            }
            hookStatus = "hook\tok";
        } catch (Throwable t) {
            hookStatus = "hook\tfail\t" + clean(String.valueOf(t));
        }
    }

    @Override
    public byte[] transform(ClassLoader l, String name, Class<?> redefined, ProtectionDomain pd, byte[] b) {
        try {
            if (redefined != null) {
                if (rewriteChannel && CHANNEL.equals(name)) {
                    try {
                        byte[] out = hookMap(b);
                        channelRewritten = true;
                        return out;
                    } catch (Throwable t) {
                        rewriteError = String.valueOf(t);
                        if (channelRewritten) {
                            hookLost = "hook\tfail\tlost on a later redefinition: " + clean(rewriteError);
                        }
                    }
                }
                return null;
            }
            if (name == null) {
                return null;
            }
            String loc = "-";
            if (pd != null && pd.getCodeSource() != null && pd.getCodeSource().getLocation() != null) {
                loc = pd.getCodeSource().getLocation().toString();
            }
            if (loc.startsWith("jrt:")) {
                return null;
            }
            if (l == null && "-".equals(loc) && jdk(name.replace('/', '.'))) {
                return null; // JDK classes of the bootstrap loader (no code source)
            }
            if (seen.putIfAbsent(name + "@" + System.identityHashCode(l), Boolean.TRUE) == null) {
                seen.putIfAbsent(name, Boolean.TRUE);
                String[] info = classInfo(b);
                classes.add("class\t" + clean(name) + "\t" + clean(loc) + "\t" + info[1] + "\t" + info[2]);
            }
        } catch (Throwable t) {
            // never break class loading
        }
        return null;
    }

    static void finish(Path log, String startLine) {
        try (PrintWriter w = new PrintWriter(Files.newBufferedWriter(log, StandardCharsets.UTF_8))) {
            w.println(startLine);
            w.println(jfrStatus);
            w.println(hookLost != null ? hookLost : hookStatus);
            synchronized (classes) {
                for (String s : classes) {
                    w.println(s);
                }
            }
            Method hidden = null;
            try {
                hidden = Class.class.getMethod("isHidden");
            } catch (Throwable t) {
                // JDK < 15: no hidden classes
            }
            for (Class<?> c : inst.getAllLoadedClasses()) {
                try {
                    if (c.isArray() || c.isPrimitive()) {
                        continue;
                    }
                    ProtectionDomain pd = c.getProtectionDomain();
                    if (pd == null || pd.getCodeSource() == null || pd.getCodeSource().getLocation() == null) {
                        continue;
                    }
                    String loc = pd.getCodeSource().getLocation().toString();
                    if (loc.startsWith("jrt:")) {
                        continue;
                    }
                    String n = c.getName().replace('.', '/');
                    if (seen.containsKey(n) || n.startsWith("bgraph/agent/") || (hidden != null && (Boolean) hidden.invoke(c)) || n.contains("$$Lambda") || n.contains("/0x")) {
                        continue;
                    }
                    w.println("unseen\t" + clean(n) + "\t" + clean(loc));
                } catch (Throwable t) {
                    // ignore one class
                }
            }
            if (drain != null) {
                for (String e : (String[]) drain.invoke(null)) {
                    w.println("mmap\t" + e);
                }
            }
            w.println("end");
        } catch (Throwable t) {
            // the missing "end" line reports the failure
        }
    }

    static String clean(String s) {
        if (s == null || s.isEmpty()) {
            return "-";
        }
        return s.replace('\t', ' ').replace('\r', ' ').replace('\n', ' ');
    }

    static boolean jdk(String c) {
        return c.startsWith("java.") || c.startsWith("javax.") || c.startsWith("jdk.") || c.startsWith("sun.") || c.startsWith("com.sun.");
    }

    // ---- class files -------------------------------------------------------------------------------------------------------

    /** {this_class internal name, SourceFile, SMAP files} of a class file, "-" when absent or unreadable. */
    static String[] classInfo(byte[] b) {
        String[] out = {"-", "-", "-"};
        try {
            DataInputStream in = new DataInputStream(new ByteArrayInputStream(b));
            in.readInt();
            in.readUnsignedShort();
            in.readUnsignedShort();
            int n = in.readUnsignedShort();
            String[] utf = new String[n];
            int[] cls = new int[n];
            for (int i = 1; i < n; i++) {
                int tag = in.readUnsignedByte();
                switch (tag) {
                    case 1: utf[i] = in.readUTF(); break;
                    case 7: cls[i] = in.readUnsignedShort(); break;
                    case 3: case 4: in.readInt(); break;
                    case 5: case 6: in.readLong(); i++; break;
                    case 8: case 16: case 19: case 20: in.readUnsignedShort(); break;
                    case 9: case 10: case 11: case 12: case 17: case 18: in.readInt(); break;
                    case 15: in.readUnsignedByte(); in.readUnsignedShort(); break;
                    default: return out;
                }
            }
            in.readUnsignedShort();
            int self = in.readUnsignedShort();
            if (self > 0 && self < n && cls[self] > 0) {
                out[0] = clean(utf[cls[self]]);
            }
            in.readUnsignedShort();
            int ifs = in.readUnsignedShort();
            for (int i = 0; i < ifs; i++) {
                in.readUnsignedShort();
            }
            for (int k = 0; k < 2; k++) { // fields, methods
                int cnt = in.readUnsignedShort();
                for (int i = 0; i < cnt; i++) {
                    in.readUnsignedShort();
                    in.readUnsignedShort();
                    in.readUnsignedShort();
                    int ac = in.readUnsignedShort();
                    for (int j = 0; j < ac; j++) {
                        in.readUnsignedShort();
                        int len = in.readInt();
                        in.readFully(new byte[len]);
                    }
                }
            }
            int ac = in.readUnsignedShort();
            for (int i = 0; i < ac; i++) {
                String an = utf[in.readUnsignedShort()];
                byte[] a = new byte[in.readInt()];
                in.readFully(a);
                if ("SourceFile".equals(an) && a.length >= 2) {
                    out[1] = clean(utf[((a[0] & 0xff) << 8) | (a[1] & 0xff)]);
                } else if ("SourceDebugExtension".equals(an)) {
                    out[2] = clean(smapFiles(new String(a, StandardCharsets.UTF_8)));
                }
            }
        } catch (Throwable t) {
            // keep what was read
        }
        return out;
    }

    /** The *F sections of an SMAP: "name|path" per file ("name|" without a path), joined by ';'. */
    static String smapFiles(String smap) {
        Set<String> files = new LinkedHashSet<>();
        boolean inF = false;
        String pending = null;
        for (String line : smap.split("\r?\n")) {
            line = line.trim();
            if (pending != null) {
                files.add(pending + "|" + line);
                pending = null;
                continue;
            }
            if (line.startsWith("*")) {
                inF = line.equals("*F");
                continue;
            }
            if (!inF || line.isEmpty()) {
                continue;
            }
            if (line.startsWith("+")) {
                pending = line.replaceFirst("^\\+\\s*\\d+\\s+", "");
            } else {
                files.add(line.replaceFirst("^\\d+\\s+", "") + "|");
            }
        }
        if (pending != null) {
            files.add(pending + "|");
        }
        return String.join(";", files);
    }

    // ---- FileChannelImpl.map rewriting ---------------------------------------------------------------------------------------

    /**
     * Inserts, at pc 0 of every instance method named "map" of FileChannelImpl:
     * aload_0; getfield FileChannelImpl.path; invokestatic bgraph/agent/Hook.map(String); nop (8 bytes: switch padding unchanged).
     */
    static byte[] hookMap(byte[] b) throws IOException {
        Cursor c = new Cursor(b);
        c.pos = 8;
        int cpCount = c.u2();
        String[] utf = new String[cpCount];
        for (int i = 1; i < cpCount; i++) {
            int tag = c.u1();
            switch (tag) {
                case 1: {
                    int len = c.u2();
                    utf[i] = new DataInputStream(new ByteArrayInputStream(b, c.pos - 2, len + 2)).readUTF();
                    c.pos += len;
                    break;
                }
                case 3: case 4: case 9: case 10: case 11: case 12: case 17: case 18: c.pos += 4; break;
                case 5: case 6: c.pos += 8; i++; break;
                case 7: case 8: case 16: case 19: case 20: c.pos += 2; break;
                case 15: c.pos += 3; break;
                default: throw new IOException("constant pool tag " + tag);
            }
        }
        int cpEnd = c.pos;
        for (String u : utf) {
            if ("bgraph/agent/Hook".equals(u)) {
                return b; // already hooked (redefinition with bytes that carry the hook)
            }
        }
        c.u2(); // access
        int thisClass = c.u2();
        c.u2(); // super
        int ifs = c.u2(); // interfaces
        c.pos += 2 * ifs;
        int fields = c.u2();
        for (int i = 0; i < fields; i++) {
            c.pos += 6;
            skipAttributes(c);
        }
        int methodsStart = c.pos;
        int base = cpCount;
        if (base + 10 > 0xffff) {
            throw new IOException("constant pool full");
        }
        int hookMethod = base + 5, pathField = base + 9;

        ByteArrayOutputStream bo = new ByteArrayOutputStream(b.length + 256);
        DataOutputStream out = new DataOutputStream(bo);
        out.write(b, 0, 8);
        out.writeShort(cpCount + 10);
        out.write(b, 10, cpEnd - 10);
        out.writeByte(1); out.writeUTF("bgraph/agent/Hook");      // base
        out.writeByte(7); out.writeShort(base);                   // base+1
        out.writeByte(1); out.writeUTF("map");                    // base+2
        out.writeByte(1); out.writeUTF("(Ljava/lang/String;)V");  // base+3
        out.writeByte(12); out.writeShort(base + 2); out.writeShort(base + 3); // base+4
        out.writeByte(10); out.writeShort(base + 1); out.writeShort(base + 4); // base+5
        out.writeByte(1); out.writeUTF("path");                   // base+6
        out.writeByte(1); out.writeUTF("Ljava/lang/String;");     // base+7
        out.writeByte(12); out.writeShort(base + 6); out.writeShort(base + 7); // base+8
        out.writeByte(9); out.writeShort(thisClass); out.writeShort(base + 8); // base+9
        out.write(b, cpEnd, methodsStart - cpEnd);

        c.pos = methodsStart;
        int methods = c.u2();
        out.writeShort(methods);
        int rewritten = 0;
        for (int i = 0; i < methods; i++) {
            int access = c.u2(), name = c.u2(), desc = c.u2(), ac = c.u2();
            out.writeShort(access); out.writeShort(name); out.writeShort(desc); out.writeShort(ac);
            boolean target = "map".equals(utf[name]) && (access & 0x0008) == 0;
            for (int j = 0; j < ac; j++) {
                int an = c.u2();
                int len = c.u4();
                byte[] data = new byte[len];
                System.arraycopy(b, c.pos, data, 0, len);
                c.pos += len;
                if (target && "Code".equals(utf[an])) {
                    data = rewriteCode(data, utf, hookMethod, pathField);
                    rewritten++;
                }
                out.writeShort(an);
                out.writeInt(data.length);
                out.write(data);
            }
        }
        out.write(b, c.pos, b.length - c.pos);
        if (rewritten == 0) {
            throw new IOException("no instance method map with code");
        }
        out.flush();
        return bo.toByteArray();
    }

    static void skipAttributes(Cursor c) {
        int ac = c.u2();
        for (int j = 0; j < ac; j++) {
            c.u2();
            int len = c.u4();
            c.pos += len;
        }
    }

    static final int SHIFT = 8;

    static byte[] rewriteCode(byte[] a, String[] utf, int hookMethod, int pathField) throws IOException {
        Cursor c = new Cursor(a);
        int maxStack = c.u2(), maxLocals = c.u2(), codeLen = c.u4();
        if (codeLen + SHIFT > 0xffff) {
            throw new IOException("code too long");
        }
        ByteArrayOutputStream bo = new ByteArrayOutputStream(a.length + SHIFT);
        DataOutputStream out = new DataOutputStream(bo);
        out.writeShort(Math.max(maxStack, 1));
        out.writeShort(maxLocals);
        out.writeInt(codeLen + SHIFT);
        out.writeByte(0x2a); // aload_0
        out.writeByte(0xb4); out.writeShort(pathField); // getfield
        out.writeByte(0xb8); out.writeShort(hookMethod); // invokestatic
        out.writeByte(0x00); // nop
        out.write(a, c.pos, codeLen);
        c.pos += codeLen;
        int exc = c.u2();
        out.writeShort(exc);
        for (int i = 0; i < exc; i++) {
            out.writeShort(pc(c.u2()));
            out.writeShort(pc(c.u2()));
            out.writeShort(pc(c.u2()));
            out.writeShort(c.u2());
        }
        int ac = c.u2();
        out.writeShort(ac);
        for (int i = 0; i < ac; i++) {
            int an = c.u2();
            int len = c.u4();
            int end = c.pos + len;
            String name = utf[an];
            ByteArrayOutputStream ab = new ByteArrayOutputStream(len + 4);
            DataOutputStream ao = new DataOutputStream(ab);
            if ("LineNumberTable".equals(name)) {
                int n = c.u2();
                ao.writeShort(n);
                for (int k = 0; k < n; k++) {
                    ao.writeShort(pc(c.u2()));
                    ao.writeShort(c.u2());
                }
            } else if ("LocalVariableTable".equals(name) || "LocalVariableTypeTable".equals(name)) {
                int n = c.u2();
                ao.writeShort(n);
                for (int k = 0; k < n; k++) {
                    ao.writeShort(pc(c.u2()));
                    ao.writeShort(c.u2());
                    ao.writeShort(c.u2());
                    ao.writeShort(c.u2());
                    ao.writeShort(c.u2());
                }
            } else if ("StackMapTable".equals(name)) {
                int n = c.u2();
                ao.writeShort(n);
                if (n > 0) {
                    int type = c.u1();
                    if (type <= 63) {
                        int d = type + SHIFT;
                        if (d <= 63) {
                            ao.writeByte(d);
                        } else {
                            ao.writeByte(251);
                            ao.writeShort(d);
                        }
                    } else if (type <= 127) {
                        int d = type - 64 + SHIFT;
                        if (d <= 63) {
                            ao.writeByte(64 + d);
                        } else {
                            ao.writeByte(247);
                            ao.writeShort(d);
                        }
                    } else if (type >= 247) {
                        ao.writeByte(type);
                        ao.writeShort(pc(c.u2()));
                    } else {
                        throw new IOException("stack map frame type " + type);
                    }
                }
                ao.write(a, c.pos, end - c.pos);
            } else {
                throw new IOException("unsupported Code attribute " + name);
            }
            c.pos = end;
            ao.flush();
            out.writeShort(an);
            out.writeInt(ab.size());
            ab.writeTo(out);
        }
        if (c.pos != a.length) {
            throw new IOException("trailing bytes in Code");
        }
        out.flush();
        return bo.toByteArray();
    }

    static int pc(int v) throws IOException {
        int r = v + SHIFT;
        if (r > 0xffff) {
            throw new IOException("pc overflow");
        }
        return r;
    }

    static final class Cursor {
        final byte[] b;
        int pos;

        Cursor(byte[] b) {
            this.b = b;
        }

        int u1() {
            return b[pos++] & 0xff;
        }

        int u2() {
            int v = ((b[pos] & 0xff) << 8) | (b[pos + 1] & 0xff);
            pos += 2;
            return v;
        }

        int u4() {
            int v = ((b[pos] & 0xff) << 24) | ((b[pos + 1] & 0xff) << 16) | ((b[pos + 2] & 0xff) << 8) | (b[pos + 3] & 0xff);
            pos += 4;
            return v;
        }
    }
}
