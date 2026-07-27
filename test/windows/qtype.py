#!/usr/bin/env python3
# Type a string into the guest via the QEMU monitor `sendkey` (reliable, unlike
# vncdotool). Runs INSIDE the container (unix socket /run/shm/monitor.sock).
import socket, time, sys

SOCK = '/run/shm/monitor.sock'

# char -> (qemu keyname, needs_shift)
BASE = {
    ' ': ('spc', 0), '\n': ('ret', 0), '\t': ('tab', 0),
    '-': ('minus', 0), '=': ('equal', 0), '[': ('bracket_left', 0),
    ']': ('bracket_right', 0), ';': ('semicolon', 0), "'": ('apostrophe', 0),
    '`': ('grave_accent', 0), '\\': ('backslash', 0), ',': ('comma', 0),
    '.': ('dot', 0), '/': ('slash', 0),
    '_': ('minus', 1), '+': ('equal', 1), '{': ('bracket_left', 1),
    '}': ('bracket_right', 1), ':': ('semicolon', 1), '"': ('apostrophe', 1),
    '~': ('grave_accent', 1), '|': ('backslash', 1), '<': ('comma', 1),
    '>': ('dot', 1), '?': ('slash', 1),
    '!': ('1', 1), '@': ('2', 1), '#': ('3', 1), '$': ('4', 1),
    '%': ('5', 1), '^': ('6', 1), '&': ('7', 1), '*': ('8', 1),
    '(': ('9', 1), ')': ('0', 1),
}

def keyfor(c):
    if c.isalpha():
        return (c.lower(), 1 if c.isupper() else 0)
    if c.isdigit():
        return (c, 0)
    if c in BASE:
        return BASE[c]
    return None

def main():
    text = sys.argv[1]
    s = socket.socket(socket.AF_UNIX)
    s.connect(SOCK)
    time.sleep(0.2)
    s.recv(4096)
    for c in text:
        k = keyfor(c)
        if not k:
            continue
        name, shift = k
        combo = ('shift-' + name) if shift else name
        s.send(('sendkey ' + combo + '\n').encode())
        time.sleep(0.02)
        s.recv(4096)  # drain echo
    s.close()

if __name__ == '__main__':
    main()
