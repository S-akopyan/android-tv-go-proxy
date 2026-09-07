import android.hardware.input.InputManager;
import android.os.SystemClock;
import android.view.InputDevice;
import android.view.InputEvent;
import android.view.KeyCharacterMap;
import android.view.KeyEvent;

import java.io.BufferedReader;
import java.io.BufferedWriter;
import java.io.ByteArrayOutputStream;
import java.io.InputStream;
import java.io.InputStreamReader;
import java.io.OutputStreamWriter;
import java.lang.reflect.Field;
import java.lang.reflect.Method;
import java.net.DatagramPacket;
import java.net.DatagramSocket;
import java.net.InetSocketAddress;
import java.net.ServerSocket;
import java.net.Socket;
import java.net.SocketTimeoutException;
import java.net.URLDecoder;
import java.nio.charset.StandardCharsets;
import java.text.SimpleDateFormat;
import java.util.ArrayList;
import java.util.Date;
import java.util.List;
import java.util.Locale;

public final class IridiKeyServer {
    private static final int INJECT_INPUT_EVENT_MODE_WAIT_FOR_RESULT = 1;
    private static final int INJECT_INPUT_EVENT_MODE_WAIT_FOR_FINISH = 2;
    private static final int DEFAULT_PORT = 17891;
    private static final int DEFAULT_KEY_INTERVAL_MS = 0;
    private static final int DEFAULT_INJECT_MODE = INJECT_INPUT_EVENT_MODE_WAIT_FOR_RESULT;
    private static final int DEFAULT_DUPLICATE_DROP_MS = 90;
    private static final int READ_IDLE_TIMEOUT_MS = 35;
    private static final int SHELL_TIMEOUT_MS = 10000;

    private final Object inputManager;
    private final Method injectInputEvent;
    private final int keyIntervalMS;
    private final int injectMode;
    private final int duplicateDropMS;
    private long lastCommandAtMS = 0;
    private String lastExecutedCommand = "";
    private long lastExecutedAtMS = 0;

    private static final class CommandResult {
        final String response;
        final String log;

        CommandResult(String response, String log) {
            this.response = response;
            this.log = log;
        }
    }

    private interface Responder {
        void send(String response) throws Exception;
    }

    private IridiKeyServer(int keyIntervalMS, int injectMode, int duplicateDropMS) throws Exception {
        this.keyIntervalMS = keyIntervalMS;
        this.injectMode = injectMode;
        this.duplicateDropMS = duplicateDropMS;
        inputManager = InputManager.class.getDeclaredMethod("getInstance").invoke(null);
        injectInputEvent = InputManager.class.getDeclaredMethod("injectInputEvent", InputEvent.class, int.class);
        injectInputEvent.setAccessible(true);
    }

    public static void main(String[] args) throws Exception {
        int port = args.length > 0 ? Integer.parseInt(args[0]) : DEFAULT_PORT;
        int keyIntervalMS = args.length > 1 ? Integer.parseInt(args[1]) : DEFAULT_KEY_INTERVAL_MS;
        int injectMode = args.length > 2 ? Integer.parseInt(args[2]) : DEFAULT_INJECT_MODE;
        int duplicateDropMS = args.length > 3 ? Integer.parseInt(args[3]) : DEFAULT_DUPLICATE_DROP_MS;
        new IridiKeyServer(keyIntervalMS, injectMode, duplicateDropMS).serve(port);
    }

    private void serve(int port) throws Exception {
        startUDPServer(port);

        ServerSocket server = new ServerSocket();
        server.setReuseAddress(true);
        server.bind(new InetSocketAddress("0.0.0.0", port));
        System.out.println("READY TCP/UDP " + port + " injectMode=" + injectMode + " duplicateDropMS=" + duplicateDropMS);

        while (true) {
            final Socket socket = server.accept();
            new Thread(new Runnable() {
                @Override
                public void run() {
                    handle(socket);
                }
            }, "iridi-key-client").start();
        }
    }

    private void startUDPServer(final int port) {
        new Thread(new Runnable() {
            @Override
            public void run() {
                DatagramSocket udp = null;
                try {
                    udp = new DatagramSocket(null);
                    udp.setReuseAddress(true);
                    udp.bind(new InetSocketAddress("0.0.0.0", port));
                    logLine("UDP ready " + port);

                    byte[] bytes = new byte[2048];
                    while (true) {
                        final DatagramPacket packet = new DatagramPacket(bytes, bytes.length);
                        udp.receive(packet);
                        final DatagramSocket udpSocket = udp;
                        final String data = new String(packet.getData(), packet.getOffset(), packet.getLength(), StandardCharsets.UTF_8);
                        logLine("UDP rx " + packet.getSocketAddress() + " bytes=" + packet.getLength());

                        StringBuilder buffer = new StringBuilder(data);
                        drainCommands(buffer, new Responder() {
                            @Override
                            public void send(String response) throws Exception {
                                byte[] out = (response + "\n").getBytes(StandardCharsets.UTF_8);
                                DatagramPacket reply = new DatagramPacket(out, out.length, packet.getAddress(), packet.getPort());
                                udpSocket.send(reply);
                            }
                        }, true);
                    }
                } catch (Exception err) {
                    System.err.println("UDP ERR " + err);
                    err.printStackTrace(System.err);
                } finally {
                    if (udp != null) {
                        udp.close();
                    }
                }
            }
        }, "iridi-key-udp").start();
    }

    private void handle(Socket socket) {
        try {
            logLine("CLIENT open " + socket.getRemoteSocketAddress());
            socket.setTcpNoDelay(true);
            socket.setSoTimeout(READ_IDLE_TIMEOUT_MS);
            InputStream input = socket.getInputStream();
            final BufferedWriter writer = new BufferedWriter(new OutputStreamWriter(socket.getOutputStream(), StandardCharsets.UTF_8));
            Responder responder = new Responder() {
                @Override
                public void send(String response) throws Exception {
                    writer.write(response);
                    writer.write('\n');
                    writer.flush();
                }
            };
            StringBuilder buffer = new StringBuilder();
            byte[] bytes = new byte[1024];

            while (true) {
                try {
                    int count = input.read(bytes);
                    if (count < 0) {
                        break;
                    }
                    buffer.append(new String(bytes, 0, count, StandardCharsets.UTF_8));
                    drainCommands(buffer, responder, false);
                } catch (SocketTimeoutException ignored) {
                    drainCommands(buffer, responder, true);
                }
            }

            drainCommands(buffer, responder, true);
        } catch (Exception err) {
            err.printStackTrace(System.err);
        } finally {
            logLine("CLIENT close " + socket.getRemoteSocketAddress());
            try {
                socket.close();
            } catch (Exception ignored) {
            }
        }
    }

    private void drainCommands(StringBuilder buffer, Responder responder, boolean flushPartial) throws Exception {
        normalizeLineBreaks(buffer);

        int newline;
        while ((newline = indexOf(buffer, "\n", 0)) >= 0) {
            String command = buffer.substring(0, newline);
            buffer.delete(0, newline + 1);
            dispatch(command, responder);
        }

        while (true) {
            int next = findNextCommandStart(buffer);
            if (next <= 0) {
                break;
            }

            String command = buffer.substring(0, next);
            buffer.delete(0, next);
            dispatch(command, responder);
        }

        if (flushPartial && buffer.toString().trim().length() > 0) {
            String command = buffer.toString();
            buffer.setLength(0);
            dispatch(command, responder);
        }
    }

    private void normalizeLineBreaks(StringBuilder buffer) {
        for (int i = 0; i < buffer.length(); i++) {
            if (buffer.charAt(i) == '\r') {
                buffer.setCharAt(i, '\n');
            }
        }
    }

    private int indexOf(StringBuilder buffer, String needle, int from) {
        return buffer.toString().indexOf(needle, from);
    }

    private int findNextCommandStart(StringBuilder buffer) {
        String value = buffer.toString();
        String trimmed = ltrim(value);
        int offset = value.length() - trimmed.length();
        int best = -1;

        if (startsFormCommand(trimmed)) {
            best = minPositive(best, value.indexOf("cmd=", offset + 1));
            best = minPositive(best, value.indexOf("type=", offset + 1));
            return best;
        }

        String[] prefixes = new String[] {
                "noreply adb shell input keyevent",
                "noreply shell input keyevent",
                "noreply input keyevent",
                "noreply keyevent",
                "adb shell input keyevent",
                "shell input keyevent",
                "input keyevent",
                "keyevent"
        };

        for (String prefix : prefixes) {
            best = minPositive(best, findValidPrefix(value, prefix, offset + 1));
        }
        return best;
    }

    private int findValidPrefix(String value, String prefix, int from) {
        int pos = value.indexOf(prefix, from);
        while (pos > 0) {
            if (isCommandStart(value, prefix, pos)) {
                return pos;
            }
            pos = value.indexOf(prefix, pos + 1);
        }
        return -1;
    }

    private boolean isCommandStart(String value, String prefix, int pos) {
        String before = value.substring(0, pos);
        if (prefix.endsWith("keyevent") && before.endsWith("input ")) {
            return false;
        }
        if (prefix.endsWith("input keyevent") && before.endsWith("shell ")) {
            return false;
        }
        if (prefix.endsWith("shell input keyevent") && before.endsWith("adb ")) {
            return false;
        }
        return true;
    }

    private String ltrim(String value) {
        int i = 0;
        while (i < value.length() && Character.isWhitespace(value.charAt(i))) {
            i++;
        }
        return value.substring(i);
    }

    private boolean startsFormCommand(String value) {
        return value.startsWith("cmd=") || value.startsWith("type=");
    }

    private int minPositive(int current, int candidate) {
        if (candidate <= 0) {
            return current;
        }
        if (current < 0 || candidate < current) {
            return candidate;
        }
        return current;
    }

    private void dispatch(String line, Responder responder) throws Exception {
        boolean reply = true;
        String command = normalizeCommand(line);
        if (command.length() == 0) {
            return;
        }
        if (command.startsWith("noreply ")) {
            reply = false;
            command = command.substring("noreply ".length()).trim();
        }

        long receivedWall = System.currentTimeMillis();
        long gap = commandGap(receivedWall);
        logLine("RX gap=" + gap + "ms reply=" + reply + " cmd=" + safeLog(command));

        long now = SystemClock.uptimeMillis();
        String canonical = canonicalKeyCommand(command);
        if (shouldDropDuplicate(canonical, now)) {
            logLine("DROP duplicate threshold=" + duplicateDropMS + "ms cmd=" + safeLog(command));
            if (reply && responder != null) {
                responder.send("OK dropped duplicate");
            }
            return;
        }

        long started = SystemClock.uptimeMillis();
        try {
            CommandResult result = execute(command);
            long elapsed = SystemClock.uptimeMillis() - started;
            markExecuted(canonical, now);
            logLine("OK dur=" + elapsed + "ms cmd=" + safeLog(command) + result.log);
            if (reply && responder != null) {
                responder.send(result.response);
            }
        } catch (Exception err) {
            long elapsed = SystemClock.uptimeMillis() - started;
            if (reply && responder != null) {
                responder.send("ERR " + (err.getMessage() == null ? err.toString() : err.getMessage()));
            }
            System.err.println("ERR " + elapsed + "ms " + command + " " + err);
            logLine("ERR dur=" + elapsed + "ms cmd=" + safeLog(command) + " err=" + safeLog(err.toString()));
        }
    }

    private synchronized boolean shouldDropDuplicate(String canonical, long now) {
        return duplicateDropMS > 0 &&
                canonical.length() > 0 &&
                canonical.equals(lastExecutedCommand) &&
                now - lastExecutedAtMS < duplicateDropMS;
    }

    private synchronized void markExecuted(String canonical, long now) {
        if (canonical.length() == 0) {
            return;
        }
        lastExecutedCommand = canonical;
        lastExecutedAtMS = now;
    }

    private String canonicalKeyCommand(String command) {
        String normalized = command.trim();
        if (normalized.startsWith("adb shell ")) {
            normalized = normalized.substring("adb shell ".length()).trim();
        }
        if (normalized.startsWith("shell ")) {
            normalized = normalized.substring("shell ".length()).trim();
        }
        if (normalized.startsWith("input ")) {
            normalized = normalized.substring("input ".length()).trim();
        }
        if (!normalized.startsWith("keyevent ")) {
            return "";
        }
        return normalized;
    }

    private synchronized long commandGap(long nowMS) {
        if (lastCommandAtMS == 0) {
            lastCommandAtMS = nowMS;
            return -1;
        }

        long gap = nowMS - lastCommandAtMS;
        lastCommandAtMS = nowMS;
        return gap;
    }

    private void logLine(String message) {
        System.out.println(timestamp() + " " + message);
    }

    private String timestamp() {
        return new SimpleDateFormat("HH:mm:ss.SSS", Locale.US).format(new Date());
    }

    private String safeLog(String value) {
        return value.replace("\r", "\\r").replace("\n", "\\n");
    }

    private String normalizeCommand(String line) throws Exception {
        String command = line.trim();
        if (!startsFormCommand(command)) {
            return command;
        }

        String[] pairs = command.split("&");
        for (String pair : pairs) {
            int equals = pair.indexOf('=');
            if (equals < 0) {
                continue;
            }

            String key = URLDecoder.decode(pair.substring(0, equals), StandardCharsets.UTF_8.name()).trim();
            if (!key.equals("cmd")) {
                continue;
            }

            return URLDecoder.decode(pair.substring(equals + 1), StandardCharsets.UTF_8.name()).trim();
        }

        return command;
    }

    private CommandResult execute(String command) throws Exception {
        if (command.equals("ping")) {
            return new CommandResult("OK pong", "");
        }

        String normalized = stripShellPrefix(command);
        if (normalized.startsWith("input ")) {
            normalized = normalized.substring("input ".length()).trim();
        }

        String[] parts = normalized.split("\\s+");
        if (parts.length >= 2 && parts[0].equals("keyevent")) {
            boolean longPress = false;
            List<Integer> keyCodes = new ArrayList<Integer>();
            for (int i = 1; i < parts.length; i++) {
                if (parts[i].equals("--longpress")) {
                    longPress = true;
                    continue;
                }
                keyCodes.add(resolveKeyCode(parts[i]));
            }

            if (keyCodes.isEmpty()) {
                throw new IllegalArgumentException("missing keycode");
            }

            for (Integer keyCode : keyCodes) {
                injectKey(keyCode.intValue(), longPress);
                if (keyIntervalMS > 0) {
                    SystemClock.sleep(keyIntervalMS);
                }
            }

            return new CommandResult("OK", "");
        }

        String shellCommand = stripShellPrefix(command);
        if (shellCommand.equals(command)) {
            throw new IllegalArgumentException("unsupported command");
        }

        return runShell(shellCommand);
    }

    private String stripShellPrefix(String command) {
        String normalized = command.trim();
        if (normalized.startsWith("adb shell ")) {
            return normalized.substring("adb shell ".length()).trim();
        }
        if (normalized.startsWith("shell ")) {
            return normalized.substring("shell ".length()).trim();
        }
        return normalized;
    }

    private CommandResult runShell(String shellCommand) throws Exception {
        Process process = Runtime.getRuntime().exec(new String[]{"sh", "-c", shellCommand});
        StreamCollector stdout = new StreamCollector(process.getInputStream());
        StreamCollector stderr = new StreamCollector(process.getErrorStream());
        stdout.start();
        stderr.start();

        long deadline = SystemClock.uptimeMillis() + SHELL_TIMEOUT_MS;
        while (true) {
            try {
                int code = process.exitValue();
                stdout.join(200);
                stderr.join(200);
                String out = stdout.text();
                String err = stderr.text();
                if (code != 0) {
                    throw new IllegalStateException("shell exit " + code + " " + trimForResponse(err.length() > 0 ? err : out));
                }
                return new CommandResult("OK" + (out.length() > 0 ? "\n" + trimForResponse(out) : ""),
                        " shell=" + safeLog(shellCommand) + " output=" + out.length() + " err=" + err.length());
            } catch (IllegalThreadStateException stillRunning) {
                if (SystemClock.uptimeMillis() >= deadline) {
                    process.destroy();
                    throw new IllegalStateException("shell timeout");
                }
                SystemClock.sleep(20);
            }
        }
    }

    private String trimForResponse(String value) {
        value = value.trim();
        if (value.length() > 1024) {
            return value.substring(0, 1024) + "...";
        }
        return value;
    }

    private static final class StreamCollector extends Thread {
        private final InputStream input;
        private final ByteArrayOutputStream output = new ByteArrayOutputStream();

        StreamCollector(InputStream input) {
            this.input = input;
        }

        @Override
        public void run() {
            byte[] buffer = new byte[512];
            try {
                int count;
                while ((count = input.read(buffer)) >= 0) {
                    output.write(buffer, 0, count);
                }
            } catch (Exception ignored) {
            }
        }

        String text() {
            return new String(output.toByteArray(), StandardCharsets.UTF_8);
        }
    }

    private int resolveKeyCode(String value) throws Exception {
        try {
            return Integer.parseInt(value);
        } catch (NumberFormatException ignored) {
        }

        String name = value;
        if (!name.startsWith("KEYCODE_")) {
            name = "KEYCODE_" + name;
        }

        Field field = KeyEvent.class.getField(name);
        return field.getInt(null);
    }

    private void injectKey(int keyCode, boolean longPress) throws Exception {
        long now = SystemClock.uptimeMillis();
        KeyEvent down = new KeyEvent(now, now, KeyEvent.ACTION_DOWN, keyCode, 0, 0,
                KeyCharacterMap.VIRTUAL_KEYBOARD, 0, 0,
                InputDevice.SOURCE_KEYBOARD);
        inject(down);

        if (longPress) {
            SystemClock.sleep(500);
            KeyEvent repeat = KeyEvent.changeTimeRepeat(down, SystemClock.uptimeMillis(), 1,
                    KeyEvent.FLAG_LONG_PRESS);
            inject(repeat);
        }

        long upTime = SystemClock.uptimeMillis();
        KeyEvent up = new KeyEvent(now, upTime, KeyEvent.ACTION_UP, keyCode, 0, 0,
                KeyCharacterMap.VIRTUAL_KEYBOARD, 0, 0,
                InputDevice.SOURCE_KEYBOARD);
        inject(up);
    }

    private void inject(InputEvent event) throws Exception {
        Object result = injectInputEvent.invoke(inputManager, event, injectMode);
        if (result instanceof Boolean && !((Boolean) result).booleanValue()) {
            throw new IllegalStateException("injectInputEvent returned false");
        }
    }
}
