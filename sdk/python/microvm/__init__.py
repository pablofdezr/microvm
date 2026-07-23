"""microvm: Python client for the microvm daemon.

    from microvm import Client

    client = Client("http://127.0.0.1:8080", token)
    sb = client.sandboxes.create("python")
    try:
        exe = client.run(sb["id"], "python3", "-c", "print('hi')")
        print(exe["stdout"], end="")
    finally:
        client.sandboxes.delete(sb["id"])
"""

from .client import APIError, Client, RequestInfo, __version__

__all__ = ["Client", "APIError", "RequestInfo", "__version__"]
