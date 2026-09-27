package ftpserver

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/secsy/goftp"
	"github.com/stretchr/testify/require"
)

func panicOnError(err error) {
	if err != nil {
		panic(err)
	}
}

func TestLoginSuccess(t *testing.T) {
	server := NewTestServer(t, false)
	// send a NOOP before the login, this doesn't seems possible using secsy/goftp so use the old way ...
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(t.Context(), "tcp", server.Addr())
	require.NoError(t, err)

	defer func() {
		err = conn.Close()
		require.NoError(t, err)
	}()

	buf := make([]byte, 1024)
	readBytes, err := conn.Read(buf)
	require.NoError(t, err)

	response := string(buf[:readBytes])
	require.Equal(t, "220 TEST Server\r\n", response)

	_, err = conn.Write([]byte("NOOP\r\n"))
	require.NoError(t, err)

	readBytes, err = conn.Read(buf)
	require.NoError(t, err)

	response = string(buf[:readBytes])
	require.Equal(t, "200 OK\r\n", response)

	conf := goftp.Config{
		User:     authUser,
		Password: authPass,
	}

	c, err := goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	defer func() { panicOnError(c.Close()) }()

	raw, err := c.OpenRawConn()
	require.NoError(t, err, "Couldn't open raw connection")

	defer func() { require.NoError(t, raw.Close()) }()

	returnCode, _, err := raw.SendCommand("NOOP")
	require.NoError(t, err)
	require.Equal(t, StatusOK, returnCode, "Couldn't NOOP")

	returnCode, response, err = raw.SendCommand("SYST")
	require.NoError(t, err)
	require.Equal(t, StatusSystemType, returnCode)
	require.Equal(t, "UNIX Type: L8", response)

	server.settings.DisableSYST = true
	returnCode, response, err = raw.SendCommand("SYST")
	require.NoError(t, err)
	require.Equal(t, StatusCommandNotImplemented, returnCode, response)
}

func TestLoginFailure(t *testing.T) {
	server := NewTestServer(t, false)

	conf := goftp.Config{
		User:     authUser,
		Password: authPass + "_wrong",
	}

	client, err := goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	defer func() { panicOnError(client.Close()) }()

	_, err = client.OpenRawConn()
	require.Error(t, err, "We should have failed to login")
}

func TestLoginCustom(t *testing.T) {
	req := require.New(t)
	driver := &MesssageDriver{}
	driver.Init()
	server := NewTestServerWithDriver(t, driver)

	conf := goftp.Config{
		User:     authUser,
		Password: authPass + "_wrong",
	}

	client, err := goftp.DialConfig(conf, server.Addr())
	req.NoError(err, "Couldn't connect")

	defer func() { panicOnError(client.Close()) }()

	_, err = client.OpenRawConn()
	req.Error(err, "We should have failed to login")
}

func TestLoginNil(t *testing.T) {
	server := NewTestServer(t, true)
	req := require.New(t)

	conf := goftp.Config{
		User:     "nil",
		Password: "nil",
	}

	client, err := goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	defer func() { panicOnError(client.Close()) }()

	_, err = client.OpenRawConn()
	req.Error(err)
}

func TestAuthTLS(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{
		Debug: false,
		TLS:   true,
	})

	conf := goftp.Config{
		User:     authUser,
		Password: authPass,
		TLSConfig: &tls.Config{
			//nolint:gosec
			InsecureSkipVerify: true,
		},
		TLSMode: goftp.TLSExplicit,
	}

	client, err := goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	defer func() { panicOnError(client.Close()) }()

	raw, err := client.OpenRawConn()
	require.NoError(t, err, "Couldn't upgrade connection to TLS")

	err = raw.Close()
	require.NoError(t, err)
}

func TestAuthExplicitTLSFailure(t *testing.T) {
	server := NewTestServer(t, false)

	conf := goftp.Config{
		User:     authUser,
		Password: authPass,
		TLSConfig: &tls.Config{
			//nolint:gosec
			InsecureSkipVerify: true,
		},
		TLSMode: goftp.TLSExplicit,
	}

	c, err := goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	defer func() { panicOnError(c.Close()) }()

	_, err = c.OpenRawConn()
	require.Error(t, err, "Upgrade to TLS should fail, TLS is not configured server side")
}

func TestAuthTLSRequired(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{
		Debug: false,
		TLS:   true,
	})
	server.settings.TLSRequired = MandatoryEncryption

	conf := goftp.Config{
		User:     authUser,
		Password: authPass,
	}

	client, err := goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	defer func() { panicOnError(client.Close()) }()

	_, err = client.OpenRawConn()
	require.Error(t, err, "Plain text login must fail, TLS is required")
	require.EqualError(t, err, "unexpected response: 421-TLS is required")

	conf.TLSConfig = &tls.Config{
		//nolint:gosec
		InsecureSkipVerify: true,
	}
	conf.TLSMode = goftp.TLSExplicit

	client, err = goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	raw, err := client.OpenRawConn()
	require.NoError(t, err, "Couldn't open raw connection")

	defer func() { require.NoError(t, raw.Close()) }()

	rc, _, err := raw.SendCommand("STAT")
	require.NoError(t, err)
	require.Equal(t, StatusSystemStatus, rc)
}

func TestAuthTLSVerificationFailed(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{
		Debug:                true,
		TLS:                  true,
		TLSVerificationReply: tlsVerificationFailed,
	})

	conf := goftp.Config{
		User:     authUser,
		Password: authPass,
		TLSConfig: &tls.Config{
			//nolint:gosec
			InsecureSkipVerify: true,
		},
		TLSMode: goftp.TLSExplicit,
	}

	client, err := goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	defer func() { panicOnError(client.Close()) }()

	_, err = client.OpenRawConn()
	require.Error(t, err, "TLS verification shoul fail")
}

func TestAuthTLSCertificate(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{
		Debug:                true,
		TLS:                  true,
		TLSVerificationReply: tlsVerificationAuthenticated,
	})

	conf := goftp.Config{
		User: authUser,
		TLSConfig: &tls.Config{
			//nolint:gosec
			InsecureSkipVerify: true,
		},
		TLSMode: goftp.TLSExplicit,
	}

	c, err := goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	defer func() { panicOnError(c.Close()) }()

	raw, err := c.OpenRawConn()
	require.NoError(t, err, "Couldn't open raw connection")

	defer func() { require.NoError(t, raw.Close()) }()

	rc, _, err := raw.SendCommand("STAT")
	require.NoError(t, err)
	require.Equal(t, StatusSystemStatus, rc)
}

func TestAuthPerClientTLSRequired(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{
		Debug:          true,
		TLS:            true,
		TLSRequirement: MandatoryEncryption,
	})

	conf := goftp.Config{
		User:     authUser,
		Password: authPass,
	}

	client, err := goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	defer func() { panicOnError(client.Close()) }()

	_, err = client.OpenRawConn()
	require.Error(t, err, "Plain text login must fail, TLS is required")
	require.EqualError(t, err, "unexpected response: 421-TLS is required")

	conf.TLSConfig = &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
	}
	conf.TLSMode = goftp.TLSExplicit

	client, err = goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	raw, err := client.OpenRawConn()
	require.NoError(t, err, "Couldn't open raw connection")

	defer func() { require.NoError(t, raw.Close()) }()

	rc, _, err := raw.SendCommand("STAT")
	require.NoError(t, err)
	require.Equal(t, StatusSystemStatus, rc)
}

func TestUserVerifierError(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{
		Debug: false,
		TLS:   true,
		// setting an invalid TLS requirement will cause the test driver
		// to return an error in PreAuthUser
		TLSRequirement: -1,
	})

	conf := goftp.Config{
		User:     authUser,
		Password: authPass,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec
		},
		TLSMode: goftp.TLSExplicit,
	}

	c, err := goftp.DialConfig(conf, server.Addr())
	require.NoError(t, err, "Couldn't connect")

	defer func() { panicOnError(c.Close()) }()

	_, err = c.OpenRawConn()
	require.Error(t, err, "Plain text login must fail, TLS is required")
	require.EqualError(t, err, "unexpected response: 530-User rejected: invalid TLS requirement")
}

// cccDialAndLogin opens a control connection, upgrades it to TLS with AUTH TLS
// and logs in. The returned tls.Conn is built on a noCloseConn so that closing
// it sends a close_notify without closing the returned raw connection.
func cccDialAndLogin(t *testing.T, addr string, tlsConfig *tls.Config) (net.Conn, *tls.Conn, *bufio.Reader) {
	t.Helper()
	req := require.New(t)

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	rawConn, err := dialer.DialContext(t.Context(), "tcp", addr)
	req.NoError(err)

	reader := bufio.NewReader(rawConn)
	line, err := reader.ReadString('\n')
	req.NoError(err)
	req.True(strings.HasPrefix(line, "220"), line)

	_, err = rawConn.Write([]byte("AUTH TLS\r\n"))
	req.NoError(err)

	line, err = reader.ReadString('\n')
	req.NoError(err)
	req.True(strings.HasPrefix(line, "234"), line)

	tlsConn := tls.Client(noCloseConn{Conn: rawConn}, tlsConfig)
	req.NoError(tlsConn.Handshake())

	tlsReader := bufio.NewReader(tlsConn)

	_, err = tlsConn.Write([]byte("USER " + authUser + "\r\n"))
	req.NoError(err)

	line, err = tlsReader.ReadString('\n')
	req.NoError(err)
	req.True(strings.HasPrefix(line, "331"), line)

	_, err = tlsConn.Write([]byte("PASS " + authPass + "\r\n"))
	req.NoError(err)

	line, err = tlsReader.ReadString('\n')
	req.NoError(err)
	req.True(strings.HasPrefix(line, "230"), line)

	return rawConn, tlsConn, tlsReader
}

func cccTLSClientConfig(maxVersion uint16) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
		MaxVersion:         maxVersion,
	}
}

func TestCCCDisabled(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{
		Debug: false,
		TLS:   true,
	})

	rawConn, tlsConn, tlsReader := cccDialAndLogin(t, server.Addr(), cccTLSClientConfig(0))
	defer func() { require.NoError(t, rawConn.Close()) }()

	_, err := tlsConn.Write([]byte("CCC\r\n"))
	require.NoError(t, err)

	line, err := tlsReader.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "502"), line)

	// the connection is still usable over TLS
	_, err = tlsConn.Write([]byte("NOOP\r\n"))
	require.NoError(t, err)

	line, err = tlsReader.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "200 OK\r\n", line)
}

func TestCCCRequiresTLS(t *testing.T) {
	server := NewTestServer(t, false)
	server.settings.EnableCCC = true

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(t.Context(), "tcp", server.Addr())
	require.NoError(t, err)

	defer func() { require.NoError(t, conn.Close()) }()

	reader := bufio.NewReader(conn)

	line, err := reader.ReadString('\n') // banner
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "220"), line)

	for _, cmd := range []string{"USER " + authUser, "PASS " + authPass, "CCC"} {
		_, err = conn.Write([]byte(cmd + "\r\n"))
		require.NoError(t, err)

		line, err = reader.ReadString('\n')
		require.NoError(t, err)

		if cmd == "CCC" {
			require.True(t, strings.HasPrefix(line, "503"), line)
		}
	}
}

func TestCCCSuccess(t *testing.T) {
	for name, maxVersion := range map[string]uint16{
		"TLS1.2": tls.VersionTLS12,
		"TLS1.3": tls.VersionTLS13,
	} {
		t.Run(name, func(t *testing.T) {
			server := NewTestServerWithTestDriver(t, &TestServerDriver{
				Debug: false,
				TLS:   true,
			})
			server.settings.EnableCCC = true

			rawConn, tlsConn, tlsReader := cccDialAndLogin(t, server.Addr(), cccTLSClientConfig(maxVersion))
			defer func() { require.NoError(t, rawConn.Close()) }()

			// data connections must stay protected after the CCC
			_, err := tlsConn.Write([]byte("PROT P\r\n"))
			require.NoError(t, err)

			line, err := tlsReader.ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "200 OK\r\n", line)

			_, err = tlsConn.Write([]byte("CCC\r\n"))
			require.NoError(t, err)

			line, err = tlsReader.ReadString('\n')
			require.NoError(t, err)
			require.True(t, strings.HasPrefix(line, "200"), line)

			// consume the server close_notify
			_, err = tlsConn.Read(make([]byte, 1))
			require.Equal(t, io.EOF, err)

			// switch to clear text: send our close_notify without closing the raw connection
			require.NoError(t, tlsConn.Close())

			// tls.Conn.Close leaves an expired write deadline on the raw connection
			require.NoError(t, rawConn.SetDeadline(time.Time{}))

			// pipeline a clear-text command right away: it must not be lost nor
			// must the close_notify be interpreted as a command
			_, err = rawConn.Write([]byte("NOOP\r\n"))
			require.NoError(t, err)

			plainReader := bufio.NewReader(rawConn)

			line, err = plainReader.ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "200 OK\r\n", line)

			// the session is preserved
			_, err = rawConn.Write([]byte("PWD\r\n"))
			require.NoError(t, err)

			line, err = plainReader.ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, "257 \"/\" is the current directory\r\n", line)
		})
	}
}

func TestCCCClientNotResponding(t *testing.T) {
	oldTimeout := cccShutdownTimeout
	cccShutdownTimeout = 200 * time.Millisecond

	defer func() { cccShutdownTimeout = oldTimeout }()

	server := NewTestServerWithTestDriver(t, &TestServerDriver{
		Debug: false,
		TLS:   true,
	})
	server.settings.EnableCCC = true

	rawConn, tlsConn, tlsReader := cccDialAndLogin(t, server.Addr(), cccTLSClientConfig(0))
	defer func() { require.NoError(t, rawConn.Close()) }()

	_, err := tlsConn.Write([]byte("CCC\r\n"))
	require.NoError(t, err)

	line, err := tlsReader.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "200"), line)

	// consume the server close_notify but don't answer: the server gives up,
	// reports the failure in clear text and closes the connection
	_, err = tlsConn.Read(make([]byte, 1))
	require.Equal(t, io.EOF, err)

	require.NoError(t, rawConn.SetReadDeadline(time.Now().Add(5*time.Second)))

	plainReader := bufio.NewReader(rawConn)

	line, err = plainReader.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "421"), line)

	_, err = plainReader.ReadString('\n')
	require.Error(t, err)
}

func TestCCCImplicitTLS(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{
		Debug: false,
		TLS:   true,
		Settings: &Settings{
			TLSRequired: ImplicitEncryption,
			EnableCCC:   true,
		},
	})

	dialer := &net.Dialer{Timeout: 5 * time.Second}
	rawConn, err := dialer.DialContext(t.Context(), "tcp", server.Addr())
	require.NoError(t, err)

	defer func() { require.NoError(t, rawConn.Close()) }()

	tlsConn := tls.Client(rawConn, cccTLSClientConfig(0))
	require.NoError(t, tlsConn.Handshake())

	reader := bufio.NewReader(tlsConn)

	line, err := reader.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "220"), line)

	_, err = tlsConn.Write([]byte("USER " + authUser + "\r\n"))
	require.NoError(t, err)

	line, err = reader.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "331"), line)

	_, err = tlsConn.Write([]byte("PASS " + authPass + "\r\n"))
	require.NoError(t, err)

	line, err = reader.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "230"), line)

	_, err = tlsConn.Write([]byte("CCC\r\n"))
	require.NoError(t, err)

	line, err = reader.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "503"), line)
}

func TestCCCNotAllowedWithMandatoryTLS(t *testing.T) {
	server := NewTestServerWithTestDriver(t, &TestServerDriver{
		Debug: false,
		TLS:   true,
	})
	server.settings.EnableCCC = true
	server.settings.TLSRequired = MandatoryEncryption

	rawConn, tlsConn, tlsReader := cccDialAndLogin(t, server.Addr(), cccTLSClientConfig(0))
	defer func() { require.NoError(t, rawConn.Close()) }()

	_, err := tlsConn.Write([]byte("CCC\r\n"))
	require.NoError(t, err)

	line, err := tlsReader.ReadString('\n')
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(line, "534"), line)
}
