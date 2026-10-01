package ftpserver

import (
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"errors"
)

const tlsBindingTokenSize = 32

var tlsBindingExtraPrefix = []byte("ftpserver-data-session-binding:")

var errTLSDataSessionNotResumed = errors.New("data connection TLS session is not resumed")

type tlsUnwrapSessionFunc func([]byte, tls.ConnectionState) (*tls.SessionState, error)

func prepareControlTLSConfig(base *tls.Config) (*tls.Config, []byte, tlsUnwrapSessionFunc, func() (*tls.Config, bool)) {
	token := make([]byte, tlsBindingTokenSize)
	_, _ = rand.Read(token)

	originalUnwrapSession := base.UnwrapSession
	controlConfig := base.Clone()
	var selectedConfig *tls.Config
	wrapControlTLSConfig(controlConfig, token, originalUnwrapSession, &selectedConfig)

	return controlConfig, token, originalUnwrapSession, func() (*tls.Config, bool) {
		if selectedConfig != nil {
			return selectedConfig, true
		}

		return controlConfig, false
	}
}

func wrapControlTLSConfig(
	config *tls.Config,
	token []byte,
	originalUnwrapSession tlsUnwrapSessionFunc,
	selectedConfig **tls.Config,
) {
	originalGetConfigForClient := config.GetConfigForClient
	config.GetConfigForClient = func(info *tls.ClientHelloInfo) (*tls.Config, error) {
		var dynamic *tls.Config
		var err error

		if originalGetConfigForClient != nil {
			dynamic, err = originalGetConfigForClient(info)
		}
		if err != nil || dynamic == nil {
			return dynamic, err
		}

		wrapControlTLSConfig(dynamic, token, originalUnwrapSession, selectedConfig)
		*selectedConfig = dynamic

		return dynamic, nil
	}
	config.UnwrapSession = func([]byte, tls.ConnectionState) (*tls.SessionState, error) {
		return nil, nil
	}
	wrapSessionWithBinding(config, token)
}

func prepareDataTLSConfig(
	base *tls.Config,
	token []byte,
	originalUnwrapSession tlsUnwrapSessionFunc,
	keepGetConfig bool,
) *tls.Config {
	config := base.Clone()

	if keepGetConfig {
		config.GetConfigForClient = nil
	} else {
		originalGetConfigForClient := config.GetConfigForClient
		config.GetConfigForClient = func(info *tls.ClientHelloInfo) (*tls.Config, error) {
			var dynamic *tls.Config
			var err error

			if originalGetConfigForClient != nil {
				dynamic, err = originalGetConfigForClient(info)
			}
			if err != nil || dynamic == nil {
				return dynamic, err
			}

			wrapDataTLSConfig(dynamic, token, originalUnwrapSession)

			return dynamic, nil
		}
	}
	wrapDataTLSConfig(config, token, originalUnwrapSession)

	return config
}

func wrapDataTLSConfig(config *tls.Config, token []byte, originalUnwrapSession tlsUnwrapSessionFunc) {
	config.UnwrapSession = func(identity []byte, state tls.ConnectionState) (*tls.SessionState, error) {
		var session *tls.SessionState
		var err error

		if originalUnwrapSession != nil {
			session, err = originalUnwrapSession(identity, state)
		} else {
			session, err = config.DecryptTicket(identity, state)
		}
		if err != nil {
			return nil, err
		}
		if session == nil || !sessionHasBinding(session, token) {
			return nil, errTLSDataSessionNotResumed
		}

		return session, nil
	}

	originalVerifyConnection := config.VerifyConnection
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if originalVerifyConnection != nil {
			if err := originalVerifyConnection(state); err != nil {
				return err
			}
		}
		if !state.DidResume {
			return errTLSDataSessionNotResumed
		}

		return nil
	}

	wrapSessionWithBinding(config, token)
}

func wrapSessionWithBinding(config *tls.Config, token []byte) {
	originalWrapSession := config.WrapSession
	config.WrapSession = func(state tls.ConnectionState, session *tls.SessionState) ([]byte, error) {
		if !sessionHasBinding(session, token) {
			session.Extra = append(session.Extra, append(tlsBindingExtraPrefix, token...))
		}

		if originalWrapSession != nil {
			return originalWrapSession(state, session)
		}

		return config.EncryptTicket(state, session)
	}
}

func sessionHasBinding(session *tls.SessionState, token []byte) bool {
	binding := append(tlsBindingExtraPrefix, token...)

	for _, extra := range session.Extra {
		if subtle.ConstantTimeCompare(extra, binding) == 1 {
			return true
		}
	}

	return false
}

func (c *clientHandler) setTLSConfig(
	controlConfig *tls.Config,
	token []byte,
	originalUnwrapSession tlsUnwrapSessionFunc,
	dataBaseConfig func() (*tls.Config, bool),
) {
	c.paramsMutex.Lock()
	defer c.paramsMutex.Unlock()

	c.tlsConfig = controlConfig
	c.tlsBindingToken = token
	c.tlsOriginalUnwrapSession = originalUnwrapSession
	c.tlsDataBaseConfig = dataBaseConfig
}

func (c *clientHandler) getTLSConfigAndBinding() (
	*tls.Config,
	[]byte,
	tlsUnwrapSessionFunc,
	func() (*tls.Config, bool),
) {
	c.paramsMutex.RLock()
	defer c.paramsMutex.RUnlock()

	return c.tlsConfig, c.tlsBindingToken, c.tlsOriginalUnwrapSession, c.tlsDataBaseConfig
}

func (c *clientHandler) getTLSDataConfig() (*tls.Config, error) {
	if !c.requiresTLSDataSessionBinding() {
		return c.server.driver.GetTLSConfig()
	}

	_, token, originalUnwrapSession, dataBaseConfig := c.getTLSConfigAndBinding()
	if dataBaseConfig == nil || len(token) != tlsBindingTokenSize {
		return nil, errTLSDataSessionNotResumed
	}

	dataBase, dynamicConfig := dataBaseConfig()

	return prepareDataTLSConfig(dataBase, token, originalUnwrapSession, dynamicConfig), nil
}
