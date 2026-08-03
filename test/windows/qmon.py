#!/usr/bin/env python3
# Send raw QEMU monitor commands (one per argument) and print the replies.
import socket, sys, time
s = socket.socket(socket.AF_UNIX); s.connect('/run/shm/monitor.sock')
time.sleep(0.3); s.recv(65536)
for cmd in sys.argv[1:]:
    s.send((cmd + '\n').encode()); time.sleep(0.4)
    try: print(s.recv(65536).decode(errors='replace'), end='')
    except Exception: pass
s.close()
