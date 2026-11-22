package bgraph.agent;

import java.io.PrintStream;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.nio.file.Path;
import java.nio.file.Paths;
import java.util.ArrayList;
import java.util.LinkedHashSet;
import java.util.List;
import java.util.Set;
import jdk.jfr.consumer.RecordedEvent;
import jdk.jfr.consumer.RecordedFrame;
import jdk.jfr.consumer.RecordedStackTrace;
import jdk.jfr.consumer.RecordingFile;

/**
 * Offline: java -cp agent.jar bgraph.agent.Dump F.jfr... prints, per recording, tab-separated lines "F.jfr\tKIND\t...":
 * read PATH CALLER, write PATH CALLER, process COMMAND CALLER, native NAME, classfile PATH THIS_CLASS SOURCEFILE SMAP (for every
 * distinct read path ending in .class that still exists), or "error MESSAGE" when the recording cannot be read.
 */
public final class Dump {
    private Dump() {}

    public static void main(String[] files) throws Exception {
        PrintStream out = new PrintStream(System.out, false, StandardCharsets.UTF_8);
        for (String f : files) {
            Set<String> lines = new LinkedHashSet<>();
            Set<String> classFiles = new LinkedHashSet<>();
            String err = null;
            try {
                for (RecordedEvent e : RecordingFile.readAllEvents(Paths.get(f))) {
                    switch (e.getEventType().getName()) {
                        case "jdk.FileRead": {
                            String p = e.getString("path");
                            lines.add("read\t" + Agent.clean(p) + "\t" + caller(e));
                            if (p != null && p.endsWith(".class")) {
                                classFiles.add(p);
                            }
                            break;
                        }
                        case "jdk.FileWrite":
                            lines.add("write\t" + Agent.clean(e.getString("path")) + "\t" + caller(e));
                            break;
                        case "jdk.ProcessStart":
                            lines.add("process\t" + Agent.clean(e.getString("command")) + "\t" + caller(e));
                            break;
                        case "jdk.NativeLibrary":
                            lines.add("native\t" + Agent.clean(e.getString("name")));
                            break;
                        default:
                    }
                }
            } catch (Throwable t) {
                err = String.valueOf(t);
            }
            String pre = Agent.clean(f) + "\t";
            List<String> cls = new ArrayList<>();
            for (String p : classFiles) {
                try {
                    Path cp = Paths.get(p);
                    if (!Files.isRegularFile(cp)) {
                        continue;
                    }
                    String[] info = Agent.classInfo(Files.readAllBytes(cp));
                    cls.add("classfile\t" + Agent.clean(p) + "\t" + info[0] + "\t" + info[1] + "\t" + info[2]);
                } catch (Throwable t) {
                    cls.add("classfile\t" + Agent.clean(p) + "\t-\t-\t-");
                }
            }
            for (String l : lines) {
                out.println(pre + l);
            }
            for (String l : cls) {
                out.println(pre + l);
            }
            if (err != null) {
                out.println(pre + "error\t" + Agent.clean(err));
            }
        }
        out.flush();
    }

    static String caller(RecordedEvent e) {
        RecordedStackTrace st = e.getStackTrace();
        if (st == null) {
            return "-";
        }
        for (RecordedFrame f : st.getFrames()) {
            if (f.getMethod() == null || f.getMethod().getType() == null) {
                continue;
            }
            String c = f.getMethod().getType().getName();
            if (Agent.jdk(c)) {
                continue;
            }
            return Agent.clean(c);
        }
        return "-";
    }
}
