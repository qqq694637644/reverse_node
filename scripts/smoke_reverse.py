#!/usr/bin/env python3
"""End-to-end smoke test for reverse_node + pinned Xray-core 26.3.27.

The Windows-side node is an Xray reverse Portal with a local SOCKS5 listener.
A second Xray process acts as the overseas Bridge and actively connects to the
Portal's Hysteria2/QUIC Brutal endpoint. TCP and SOCKS5 UDP are both exercised.
This script uses only Python's standard library.
"""
import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
import signal
import socket
import struct
import subprocess
import tempfile
import threading
import time
from pathlib import Path


def free_port(sock_type):
    with socket.socket(socket.AF_INET, sock_type) as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def read_exact(sock, count):
    result = bytearray()
    while len(result) < count:
        chunk = sock.recv(count - len(result))
        if not chunk:
            raise RuntimeError("SOCKS5 connection closed prematurely")
        result.extend(chunk)
    return bytes(result)


def read_socks_bound_address(sock, reply):
    atyp = reply[3]
    if atyp == 1:
        host = socket.inet_ntoa(read_exact(sock, 4))
    elif atyp == 4:
        host = socket.inet_ntop(socket.AF_INET6, read_exact(sock, 16))
    elif atyp == 3:
        length = read_exact(sock, 1)[0]
        host = read_exact(sock, length).decode("ascii")
    else:
        raise RuntimeError(f"Unsupported SOCKS5 address type {atyp}")
    port = struct.unpack("!H", read_exact(sock, 2))[0]
    return host, port


class HTTPHandler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = b"REVERSE-NODE-PORTAL-BRIDGE-TCP-PASS"
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_):
        pass


def socks_greeting(sock):
    sock.sendall(b"\x05\x01\x00")
    if read_exact(sock, 2) != b"\x05\x00":
        raise RuntimeError("SOCKS5 no-auth greeting failed")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--xray", required=True, type=Path)
    parser.add_argument("--reverse-node", required=True, type=Path)
    args = parser.parse_args()
    xray = str(args.xray.resolve())
    reverse_node = str(args.reverse_node.resolve())
    for executable in (xray, reverse_node):
        if not Path(executable).is_file():
            parser.error(f"executable not found: {executable}")

    version = subprocess.check_output([xray, "version"], text=True)
    if "Xray 26.3.27" not in version or "d2758a0" not in version:
        raise AssertionError("Smoke test requires Xray-core 26.3.27 commit d2758a0")

    with tempfile.TemporaryDirectory(prefix="reverse-node-smoke-") as temporary:
        root = Path(temporary)
        state = root / "state"
        hysteria_port = free_port(socket.SOCK_DGRAM)
        socks_port = free_port(socket.SOCK_STREAM)

        init = subprocess.run(
            [
                reverse_node, "init",
                "--xray-path", xray,
                "--public-host", "localhost",
                "--listen", "127.0.0.1",
                "--port", str(hysteria_port),
                "--socks-listen", "127.0.0.1",
                "--socks-port", str(socks_port),
                "--state-dir", str(state),
                "--log-level", "debug",
                "--brutal-up", "16mbps",
                "--brutal-down", "24mbps",
            ],
            capture_output=True, text=True, timeout=30,
        )
        if init.returncode:
            raise RuntimeError("reverse_node init failed:\n" + init.stdout + init.stderr)

        fragment = json.loads((state / "overseas-bridge.fragment.json").read_text())
        bridge_config = {
            "log": {"loglevel": "debug"},
            "reverse": fragment["reverse"],
            "routing": fragment["routing"],
            "inbounds": [],
            "outbounds": fragment["outbounds"] + [
                {"tag": "freedom", "protocol": "freedom", "settings": {}}
            ],
        }
        bridge_config_path = root / "overseas-bridge.json"
        bridge_config_path.write_text(json.dumps(bridge_config, indent=2))
        checked = subprocess.run(
            [xray, "run", "-test", "-c", str(bridge_config_path)],
            capture_output=True, text=True, timeout=30,
        )
        if checked.returncode:
            raise RuntimeError("generated Bridge config rejected:\n" + checked.stdout + checked.stderr)

        http_server = ThreadingHTTPServer(("127.0.0.1", 0), HTTPHandler)
        threading.Thread(target=http_server.serve_forever, daemon=True).start()
        udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        udp.bind(("127.0.0.1", 0))
        udp.settimeout(0.2)
        udp_port = udp.getsockname()[1]
        stop_echo = threading.Event()

        def udp_echo():
            while not stop_echo.is_set():
                try:
                    packet, peer = udp.recvfrom(4096)
                    udp.sendto(b"REVERSE-NODE-PORTAL-BRIDGE-UDP-PASS:" + packet, peer)
                except socket.timeout:
                    continue
                except OSError:
                    return

        threading.Thread(target=udp_echo, daemon=True).start()
        processes = []
        success = False
        try:
            portal_log = (root / "portal.log").open("wb")
            portal = subprocess.Popen(
                [
                    reverse_node, "run",
                    "--xray-path", xray,
                    "--state-dir", str(state),
                    "--restart=false",
                ],
                stdout=portal_log, stderr=subprocess.STDOUT,
            )
            processes.append(("portal", portal, portal_log))
            time.sleep(0.5)
            if portal.poll() is not None:
                raise RuntimeError("reverse_node Portal exited during startup")

            bridge_log = (root / "bridge.log").open("wb")
            bridge = subprocess.Popen(
                [xray, "run", "-c", str(bridge_config_path)],
                stdout=bridge_log, stderr=subprocess.STDOUT,
            )
            processes.append(("bridge", bridge, bridge_log))

            deadline = time.monotonic() + 15
            while time.monotonic() < deadline:
                for name, proc, _ in processes:
                    if proc.poll() is not None:
                        raise RuntimeError(f"{name} exited before reverse registration")
                portal_text = (root / "portal.log").read_text(errors="replace")
                bridge_text = (root / "bridge.log").read_text(errors="replace")
                if (
                    "congestion brutal bytes per second" in portal_text.lower()
                    and "congestion brutal bytes per second" in bridge_text.lower()
                ):
                    break
                time.sleep(0.2)
            else:
                raise RuntimeError("Bridge did not register with the Portal using QUIC Brutal")

            # TCP request originates from the domestic SOCKS5 listener, but the
            # destination connection must be dialed by the overseas Bridge.
            with socket.create_connection(("127.0.0.1", socks_port), timeout=8) as conn:
                conn.settimeout(10)
                socks_greeting(conn)
                conn.sendall(
                    b"\x05\x01\x00\x01" + socket.inet_aton("127.0.0.1")
                    + struct.pack("!H", http_server.server_address[1])
                )
                reply = read_exact(conn, 4)
                if reply[1] != 0:
                    raise RuntimeError(f"SOCKS5 TCP CONNECT failed: {reply!r}")
                read_socks_bound_address(conn, reply)
                conn.sendall(b"GET / HTTP/1.0\r\nHost: localhost\r\n\r\n")
                response = bytearray()
                while True:
                    chunk = conn.recv(4096)
                    if not chunk:
                        break
                    response.extend(chunk)
                if b"200 OK" not in response or b"REVERSE-NODE-PORTAL-BRIDGE-TCP-PASS" not in response:
                    raise RuntimeError("TCP response did not traverse Portal -> Bridge")

            # UDP ASSOCIATE must also traverse the reverse portal, not fall back
            # to a local/direct outbound.
            with socket.create_connection(("127.0.0.1", socks_port), timeout=8) as control:
                control.settimeout(10)
                socks_greeting(control)
                control.sendall(
                    b"\x05\x03\x00\x01" + socket.inet_aton("127.0.0.1") + b"\x00\x00"
                )
                reply = read_exact(control, 4)
                if reply[1] != 0:
                    raise RuntimeError(f"SOCKS5 UDP ASSOCIATE failed: {reply!r}")
                relay_host, relay_port = read_socks_bound_address(control, reply)
                if relay_host in ("0.0.0.0", "::"):
                    relay_host = "127.0.0.1"
                with socket.socket(socket.AF_INET, socket.SOCK_DGRAM) as udp_client:
                    udp_client.bind(("127.0.0.1", 0))
                    udp_client.settimeout(10)
                    payload = b"reverse-node-udp-smoke"
                    packet = (
                        b"\x00\x00\x00\x01" + socket.inet_aton("127.0.0.1")
                        + struct.pack("!H", udp_port) + payload
                    )
                    udp_client.sendto(packet, (relay_host, relay_port))
                    response, _ = udp_client.recvfrom(4096)
                    if b"REVERSE-NODE-PORTAL-BRIDGE-UDP-PASS:" + payload not in response:
                        raise RuntimeError("UDP response did not traverse Portal -> Bridge")

            success = True
            print("reverse_node Portal -> Hysteria2 QUIC Brutal -> overseas Bridge TCP: PASS")
            print("reverse_node Portal -> Hysteria2 QUIC Brutal -> overseas Bridge UDP: PASS")
            print("Both Xray endpoints logged actual Brutal activation.")
        finally:
            stop_echo.set()
            udp.close()
            http_server.shutdown()
            for name, proc, _ in processes:
                if proc.poll() is None:
                    if name == "portal":
                        if os.name == "nt":
                            # The wrapper owns an Xray child; taskkill /T avoids
                            # leaving that child holding the temporary log open.
                            subprocess.run(
                                ["taskkill", "/PID", str(proc.pid), "/T", "/F"],
                                capture_output=True, check=False,
                            )
                        else:
                            proc.send_signal(signal.SIGINT)
                    else:
                        proc.terminate()
            for name, proc, log_file in processes:
                try:
                    proc.wait(timeout=6)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait(timeout=5)
                log_file.close()
                log = (root / f"{name}.log").read_text(errors="replace")
                auth = fragment["outbounds"][0]["streamSettings"]["hysteriaSettings"]["auth"]
                log = log.replace(auth, "[REDACTED]")
                if not success:
                    print(f"{name} log tail:\n{log[-3500:]}")
                if success and "congestion brutal bytes per second" not in log.lower():
                    raise AssertionError(f"{name} did not log actual Brutal activation")


if __name__ == "__main__":
    main()
