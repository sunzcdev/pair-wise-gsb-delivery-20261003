# go-plugin Internals

This section discusses the internals of how go-plugin works.

go-plugin operates by either _serving_ a plugin or being a _client_
connecting to a remote plugin. The "client" is the host process or the
process that itself uses plugins. The "server" is the plugin process.

For a server:

  1. Send the handshake over the dedicated handshake channel, or output it
     to stdout for older clients
  2. Wait for connection on control address
  3. Serve plugins over control address

For a client:

  1. Launch a plugin binary
  2. Read and verify the handshake from the dedicated handshake channel, or
     from the plugin's stdout for older plugins
  3. Connect to plugin control address using desired protocol
  4. Dispense plugins using control connection

## Handshake

The handshake is the initial communication between a plugin and a host
process to determine how the host process can connect and communicate to
the plugin. Newer clients and plugins perform this handshake over a
dedicated channel (see below); otherwise it is done over the plugin
process's stdout.

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

## Handshake Channel

By default, newer clients offer a dedicated handshake channel so that the
handshake does not depend on the plugin's stdout, which plugins and their
dependencies may write to at any time. Before starting the plugin, the
client creates a listener (a unix socket, or loopback TCP with an ephemeral
port on Windows) and generates a random one-time token for the launch. It
passes both to the plugin through environment variables:

 * `PLUGIN_HANDSHAKE_NETWORK`: the network type of the channel, "unix" or "tcp".

 * `PLUGIN_HANDSHAKE_ADDR`: the address of the client's handshake listener.

 * `PLUGIN_HANDSHAKE_TOKEN`: the one-time credential for this launch.

To perform the handshake over the channel, the plugin connects to the
listener and sends a single line, terminated with a newline:

```
TOKEN|CORE-PROTOCOL-VERSION|APP-PROTOCOL-VERSION|NETWORK-TYPE|NETWORK-ADDR|PROTOCOL
```

The client only accepts the handshake line if the token matches the one it
generated for this launch; connections presenting a missing, incorrect, or
stale token are rejected, and the client keeps waiting for the correct
plugin instance. Once the handshake completes (or the start fails), the
client closes the channel and removes any resources it created.

If the environment variables are not set, the plugin should use the legacy
stdout handshake instead. Setting `DisableHandshakeSocket` on `ClientConfig`
or `ServeConfig` forces the legacy stdout handshake explicitly.

## Environment Variables

When serving a plugin over TCP, the following environment variables can be
specified to restrict the port that will be assigned to be from within a
specific range. If not values are provided, the port will be randomly assigned
by the operating system.

 * `PLUGIN_MIN_PORT`: Specifies the minimum port value that will be assigned to the listener.
 
 * `PLUGIN_MAX_PORT`: Specifies the maximum port value that will be assigned to the listener.
