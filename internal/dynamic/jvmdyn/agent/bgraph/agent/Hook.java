package bgraph.agent;

import java.util.ArrayList;
import java.util.List;

/**
 * Called from the start of every sun.nio.ch.FileChannelImpl.map method (bytecode inserted by Agent). This class is loaded by
 * the bootstrap class loader (the agent jar is appended to the bootstrap search) so that java.base can link to it; it must not
 * reference any other class of the agent.
 */
public final class Hook {
    private static final List<String> events = new ArrayList<>();

    private Hook() {}

    /** Records one mapping: the channel's path (null for a channel without one) and the first non-JDK caller. */
    public static void map(String path) {
        try {
            String caller = "-";
            for (StackTraceElement e : Thread.currentThread().getStackTrace()) {
                String c = e.getClassName();
                if (c.equals("bgraph.agent.Hook") || jdk(c)) {
                    continue;
                }
                caller = c;
                break;
            }
            String line = clean(path) + "\t" + clean(caller);
            synchronized (events) {
                events.add(line);
            }
        } catch (Throwable t) {
            synchronized (events) {
                events.add("-\t-");
            }
        }
    }

    /** Returns and clears the recorded mappings ("path\tcaller" each). */
    public static String[] drain() {
        synchronized (events) {
            String[] a = events.toArray(new String[0]);
            events.clear();
            return a;
        }
    }

    static boolean jdk(String c) {
        return c.startsWith("java.") || c.startsWith("javax.") || c.startsWith("jdk.") || c.startsWith("sun.") || c.startsWith("com.sun.");
    }

    static String clean(String s) {
        if (s == null || s.isEmpty()) {
            return "-";
        }
        return s.replace('\t', ' ').replace('\r', ' ').replace('\n', ' ');
    }
}
