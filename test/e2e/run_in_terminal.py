"""Run a command in a pseudo-terminal of 40 rows x 120 columns and print its output.

The Docker CLI only sends a console size, and resizes, when it runs in a
terminal; this gives it one.
"""
import fcntl, os, pty, select, struct, sys, termios, time

pid, fd = pty.fork()
if pid == 0:
    fcntl.ioctl(0, termios.TIOCSWINSZ, struct.pack("HHHH", 40, 120, 0, 0))
    os.execvp(sys.argv[1], sys.argv[1:])
out, deadline = b"", time.time() + 30
while time.time() < deadline:
    if select.select([fd], [], [], 1)[0]:
        try:
            data = os.read(fd, 1024)
        except OSError:
            break
        if not data:
            break
        out += data
os.waitpid(pid, 0)
sys.stdout.write(out.decode(errors="replace"))
