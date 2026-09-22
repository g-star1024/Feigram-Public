#!/usr/bin/env python3
"""代理桩：支持 SOCKS5 与 HTTP CONNECT，把每次被请求的目标写入日志文件。

用途：核证「MTProto 建连是否真的走了代理」。
桩只记录目标并立刻回失败（SOCKS5 rep=0x05 / HTTP 502），
因为要证明的是「流量被送到代理」而不是「隧道能通」。
"""
import argparse
import socket
import struct
import threading


def log(path, line):
    with open(path, "a", encoding="utf-8") as handle:
        handle.write(line + "\n")
        handle.flush()


def read_exact(conn, count):
    buf = b""
    while len(buf) < count:
        chunk = conn.recv(count - len(buf))
        if not chunk:
            raise ConnectionError("eof")
        buf += chunk
    return buf


def handle_socks5(conn, log_path):
    read_exact(conn, 1)
    method_count = read_exact(conn, 1)[0]
    read_exact(conn, method_count)
    conn.sendall(b"\x05\x00")
    read_exact(conn, 1)
    cmd = read_exact(conn, 1)[0]
    read_exact(conn, 1)
    atyp = read_exact(conn, 1)[0]
    if atyp == 1:
        host = socket.inet_ntoa(read_exact(conn, 4))
    elif atyp == 3:
        host = read_exact(conn, read_exact(conn, 1)[0]).decode()
    elif atyp == 4:
        host = socket.inet_ntop(socket.AF_INET6, read_exact(conn, 16))
    else:
        return
    port = struct.unpack("!H", read_exact(conn, 2))[0]
    log(log_path, "socks5 CONNECT %s:%d" % (host, port))
    conn.sendall(b"\x05\x05\x00\x01" + b"\x00" * 6)


def handle_http(conn, log_path):
    data = b""
    while b"\r\n\r\n" not in data:
        chunk = conn.recv(4096)
        if not chunk:
            return
        data += chunk
        if len(data) > 65536:
            return
    line = data.split(b"\r\n", 1)[0].decode(errors="replace")
    parts = line.split()
    if len(parts) >= 2 and parts[0].upper() == "CONNECT":
        log(log_path, "http CONNECT %s" % parts[1])
        conn.sendall(b"HTTP/1.1 502 Bad Gateway\r\n\r\n")
    else:
        log(log_path, "http OTHER %s" % line)
        conn.sendall(b"HTTP/1.1 405 Method Not Allowed\r\n\r\n")


def serve(port, mode, log_path):
    listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    listener.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    listener.bind(("127.0.0.1", port))
    listener.listen(64)
    print("stub ready mode=%s port=%d" % (mode, port), flush=True)
    handler = handle_socks5 if mode == "socks5" else handle_http
    while True:
        conn, _ = listener.accept()

        def run(client=conn):
            try:
                handler(client, log_path)
            except Exception:
                pass
            finally:
                try:
                    client.close()
                except Exception:
                    pass

        threading.Thread(target=run, daemon=True).start()


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, required=True)
    parser.add_argument("--mode", choices=["socks5", "http"], required=True)
    parser.add_argument("--log", required=True)
    args = parser.parse_args()
    serve(args.port, args.mode, args.log)
