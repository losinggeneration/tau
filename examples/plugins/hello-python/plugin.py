#!/usr/bin/env python3
"""A minimal tau plugin implemented in Python.

This file intentionally implements the small part of HashiCorp go-plugin's
startup protocol that tau needs, then serves tau's Plugin gRPC service.
"""

from __future__ import annotations

import base64
import json
import os
import signal
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
from concurrent import futures
from pathlib import Path

import grpc

from go_plugin_proto import grpc_broker_pb2_grpc, grpc_controller_pb2, grpc_controller_pb2_grpc, grpc_stdio_pb2_grpc
from tau_plugin_proto import plugin_pb2, plugin_pb2_grpc

API_VERSION = "1"
PLUGIN_NAME = "hello-python"


class HelloPlugin(plugin_pb2_grpc.PluginServicer):
    def __init__(self) -> None:
        self._lock = threading.Lock()
        self._init_info = None
        self._events: list[str] = []

    def Initialize(self, request, context):
        with self._lock:
            self._init_info = request
        return plugin_pb2.InitResponse(api_version=API_VERSION)

    def ListRegistrations(self, request, context):
        return plugin_pb2.Registrations(
            tools=[
                plugin_pb2.ToolRegistration(
                    name="hello_python",
                    description="Return a greeting from the example Python plugin.",
                    parameters={
                        "type": "object",
                        "properties": json.dumps(
                            {"name": {"type": "string", "description": "Optional name to greet."}}
                        ),
                    },
                )
            ],
            commands=[
                plugin_pb2.CommandRegistration(
                    name="hello-python",
                    description="Run the Python hello-world example plugin.",
                    argument_hint="[name]",
                )
            ],
            event_subscriptions=["session_start", "session_shutdown"],
            cli_flags=[
                plugin_pb2.CLIFlag(
                    name="hello-python-target",
                    description="Default name used by the Python hello-world example plugin.",
                    default_value="tau",
                )
            ],
            tool_prefix="",
        )

    def OnEvent(self, request, context):
        with self._lock:
            self._events.append(request.name)
        return plugin_pb2.EventResponse()

    def ExecuteTool(self, request, context):
        name = "tau"
        if request.arguments.strip():
            try:
                args = json.loads(request.arguments)
            except json.JSONDecodeError as exc:
                return plugin_pb2.ToolResult(is_error=True, error_message=f"parse arguments: {exc}")
            candidate = str(args.get("name", "")).strip()
            if candidate:
                name = candidate

        return plugin_pb2.ToolResult(
            output=f"Hello, {name}! This came from the tau Python example plugin."
        )

    def ExecuteCommand(self, request, context):
        name = request.arguments.strip() or "tau"
        with self._lock:
            init_info = self._init_info
            events = list(self._events)

        details: list[str] = []
        if init_info is not None:
            details.append(f"cwd={init_info.cwd}")
            if init_info.session_id:
                details.append(f"session={init_info.session_id}")
        if events:
            details.append(f"events={','.join(events)}")

        output = f"Hello, {name}! The Python hello-world plugin is loaded and responding."
        if details:
            output += "\n" + "\n".join(details)
        return plugin_pb2.CommandResult(output=output)

    def Shutdown(self, request, context):
        threading.Thread(target=_delayed_exit, daemon=True).start()
        return plugin_pb2.ShutdownResponse(graceful=True)


class Controller(grpc_controller_pb2_grpc.GRPCControllerServicer):
    def Shutdown(self, request, context):
        threading.Thread(target=_delayed_exit, daemon=True).start()
        return grpc_controller_pb2.Empty()


class Broker(grpc_broker_pb2_grpc.GRPCBrokerServicer):
    def StartStream(self, request_iterator, context):
        # tau does not currently use go-plugin's gRPC broker for this example.
        for _ in request_iterator:
            if not context.is_active():
                break
        return
        yield


def _delayed_exit() -> None:
    time.sleep(0.1)
    os._exit(0)


def _check_cookie() -> None:
    if os.environ.get("TAU_PLUGIN_API_VERSION") != API_VERSION:
        print(
            "This program is a tau plugin and is meant to be launched by tau.",
            file=sys.stderr,
        )
        sys.exit(1)


def _make_server_credentials() -> tuple[grpc.ServerCredentials | None, str]:
    client_cert = os.environ.get("PLUGIN_CLIENT_CERT")
    if not client_cert:
        return None, ""

    with tempfile.TemporaryDirectory(prefix="tau-python-plugin-cert-") as tmp_dir:
        key_path = Path(tmp_dir) / "server.key"
        cert_path = Path(tmp_dir) / "server.crt"
        subprocess.run(
            [
                "openssl",
                "req",
                "-x509",
                "-newkey",
                "rsa:2048",
                "-nodes",
                "-keyout",
                str(key_path),
                "-out",
                str(cert_path),
                "-subj",
                "/CN=localhost",
                "-addext",
                "subjectAltName=DNS:localhost",
                "-days",
                "1",
            ],
            check=True,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
        private_key = key_path.read_bytes()
        certificate = cert_path.read_bytes()

    credentials = grpc.ssl_server_credentials(
        [(private_key, certificate)],
        require_client_auth=False,
    )
    der_cert = ssl.PEM_cert_to_DER_cert(certificate.decode())
    encoded_cert = base64.b64encode(der_cert).decode().rstrip("=")
    return credentials, encoded_cert


def _listen_address() -> tuple[str, str]:
    if sys.platform == "win32":
        sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        sock.bind(("127.0.0.1", 0))
        _, port = sock.getsockname()
        sock.close()
        return "tcp", f"127.0.0.1:{port}"

    handle, path = tempfile.mkstemp(prefix="plugin")
    os.close(handle)
    os.unlink(path)
    return "unix", path


def main() -> None:
    _check_cookie()

    network, address = _listen_address()
    credentials, server_cert = _make_server_credentials()

    server = grpc.server(futures.ThreadPoolExecutor(max_workers=8))
    plugin_pb2_grpc.add_PluginServicer_to_server(HelloPlugin(), server)
    grpc_controller_pb2_grpc.add_GRPCControllerServicer_to_server(Controller(), server)
    grpc_broker_pb2_grpc.add_GRPCBrokerServicer_to_server(Broker(), server)

    # go-plugin starts a stdio stream even if tau discards it, so register a
    # placeholder implementation. The generated base implementation returns
    # UNIMPLEMENTED; that is sufficient for this example because tau does not
    # use synced plugin stdio.
    grpc_stdio_pb2_grpc.add_GRPCStdioServicer_to_server(
        grpc_stdio_pb2_grpc.GRPCStdioServicer(), server
    )

    bind_target = address if network == "tcp" else f"unix://{address}"
    if credentials is None:
        bound_port = server.add_insecure_port(bind_target)
    else:
        bound_port = server.add_secure_port(bind_target, credentials)
    if bound_port == 0 and network == "tcp":
        raise RuntimeError(f"failed to bind {bind_target}")

    server.start()
    print(f"1|1|{network}|{address}|grpc|{server_cert}", flush=True)

    stop_event = threading.Event()

    def stop(signum, frame):
        stop_event.set()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)

    try:
        while not stop_event.wait(3600):
            pass
    finally:
        server.stop(grace=1)
        if network == "unix":
            try:
                os.unlink(address)
            except FileNotFoundError:
                pass


if __name__ == "__main__":
    main()
