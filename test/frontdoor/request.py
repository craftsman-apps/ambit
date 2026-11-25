"""One HTTPS request to the public Gateway, optionally behind a PROXY protocol v1 header (test/stepfd.sh).

Usage: request.py PORT CA_FILE HOST PATH [--proxy CLIENT_IP] [--header 'Name: value']... [--post JSON]
Prints the HTTP status code (a 200 whose body holds a JSON-RPC "error" prints "error200"), or "refused" when the connection
is closed or the TLS handshake fails.
A PROXY protocol header is how a load balancer tells the gateway the real client address; curl cannot send a
made-up address, so this test client does. Test only.
"""
import socket
import ssl
import sys


def main(argv):
    port, ca, host, path = int(argv[0]), argv[1], argv[2], argv[3]
    proxy, headers, body = None, [], None
    rest = argv[4:]
    while rest:
        flag, value, rest = rest[0], rest[1], rest[2:]
        if flag == "--proxy":
            proxy = value
        elif flag == "--header":
            headers.append(value)
        elif flag == "--post":
            body = value
        else:
            raise SystemExit("unknown flag " + flag)
    try:
        raw = socket.create_connection(("127.0.0.1", port), timeout=10)
        if proxy:
            raw.sendall(("PROXY TCP4 %s 10.0.0.1 51000 %d\r\n" % (proxy, port)).encode())
        ctx = ssl.create_default_context(cafile=ca)
        tls = ctx.wrap_socket(raw, server_hostname=host)
        if body is None:
            request = "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n%s\r\n" % (path, host, "".join(h + "\r\n" for h in headers))
        else:
            payload = body.encode()
            request = "POST %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\nContent-Type: application/json\r\nAccept: application/json, text/event-stream\r\nContent-Length: %d\r\n%s\r\n" % (
                path, host, len(payload), "".join(h + "\r\n" for h in headers))
            request += body
        tls.sendall(request.encode())
        data = b""
        tls.settimeout(3)  # an event stream may stay open: what arrived by then is the answer
        try:
            while True:
                chunk = tls.recv(65536)
                if not chunk:
                    break
                data += chunk
        except (socket.timeout, TimeoutError):
            pass
        line = data.split(b"\r\n")[0].decode(errors="replace")
        status = line.split(" ")[1] if line.startswith("HTTP/") else "refused"
        if status == "200" and b'"error"' in data.split(b"\r\n\r\n", 1)[-1]:
            status = "error200"
        print(status)
    except (OSError, ssl.SSLError):
        print("refused")


main(sys.argv[1:])
