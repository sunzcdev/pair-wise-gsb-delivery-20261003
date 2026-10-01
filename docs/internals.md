# go-plugin Internals

This section discusses the internals of how go-plugin works.

go-plugin operates by either _serving_ a plugin or being a _client_
connecting to a remote plugin. The "client" is the host process or the
process that itself uses plugins. The "server" is the plugin process.

For a server:

  1. Output handshake to stdout
  2. Wait for connection on control address
  3. Serve plugins over control address

For a client:

  1. Launch a plugin binary
  2. Read and verify handshake from plugin stdout
  3. Connect to plugin control address using desired protocol
  4. Dispense plugins using control connection

## Handshake

The handshake is the initial communication between a plugin and a host
process to determine how the host process can connect and communicate to
the plugin. This handshake is done over the plugin process's stdout.

The `go-plugin` library itself handles the handshake when using the
`Server` to serve a plugin. **You do not need to understand the internals
of the handshake,** unless you're building a go-plugin compatible plugin
in another language.

The handshake is a single line of data terminated with a newline character
`\n`. It looks like the following:

```
1|3|unix|/path/to/socket|grpc
```

The structure is:

```
CORE-PROTOCOL-VERSION | APP-PROTOCOL-VERSION | NETWORK-TYPE | NETWORK-ADDR | PROTOCOL
```

Where:

  * `CORE-PROTOCOL-VERSION` is the protocol version for go-plugin itself.
    The current value is `1`. Please use this value. Any other value will
    cause your plugin to not load.

  * `APP-PROTOCOL-VERSION` is the protocol version for the application data.
    This is determined by the application. You must reference the documentation
    for your application to determine the desired value.

  * `NETWORK-TYPE` and `NETWORK-ADDR` are the networking information for
    connecting to this plugin. The type must be "unix" or "tcp". The address
    is a path to the Unix socket for "unix" and an IP address for "tcp".

  * `PROTOCOL` is the named protocol that the connection will use. If this
    is omitted (older versions), this is "netrpc" for Go net/rpc. This can
    also be "grpc". This is the protocol that the plugin wants to speak to
    the host process with.

Per-plugin version negotiation adds two optional trailing segments that are
only present when the host sent `PLUGIN_PLUGIN_VERSIONS`. Segment 6 is the
server certificate slot (empty in plaintext mode), segment 7 is `true` when
gRPC broker multiplexing is supported, and segment 8 is the negotiated
`name=version` list, comma separated (for example `auth=2,storage=1`). Plugins
present only as optional on one side are omitted; a required plugin without a
common version prevents the plugin process from starting. Clients that do not
send `PLUGIN_PLUGIN_VERSIONS` receive the legacy line above unchanged.

## Environment Variables

When serving a plugin over TCP, the following environment variables can be
specified to restrict the port that will be assigned to be from within a
specific range. If not values are provided, the port will be randomly assigned
by the operating system.

 * `PLUGIN_MIN_PORT`: Specifies the minimum port value that will be assigned to the listener.
 
 * `PLUGIN_MAX_PORT`: Specifies the maximum port value that will be assigned to the listener.

 * `PLUGIN_PROTOCOL_VERSIONS`: Comma separated list of global application
   protocol versions supported by the host.

 * `PLUGIN_PLUGIN_VERSIONS`: Per-plugin version catalog used when services in
   one process upgrade independently. Each semicolon separated entry is
   `name=<required-flag>:<version>,...` where the flag is `1` for a required
   service and `0` for an optional one, for example
   `auth=1:1,2;storage=1:1`.
