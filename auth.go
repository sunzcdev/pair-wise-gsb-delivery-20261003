// Go MySQL Driver - A MySQL-Driver for Go's database/sql package
//
// Copyright 2018 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

package mysql

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"sync"

	"filippo.io/edwards25519"
)

// CredentialsProvider supplies authentication material for each factor of a
// (multi-factor) authentication handshake.
//
// The driver calls Credentials every time the server requests authentication
// material. factor is the 1-based authentication factor being verified and
// plugin is the authentication plugin the server requested for it. The
// returned string is the authentication material for that factor, usually a
// password.
//
// Each factor is queried separately: the material of the first factor (e.g.
// Config.Passwd) is never reused for subsequent factors. If no material is
// available for a requested factor, return an error (e.g. ErrNoCredentials)
// to abort the authentication.
type CredentialsProvider interface {
	Credentials(ctx context.Context, factor int, plugin string) (string, error)
}

// CredentialsProviderFunc is an adapter to allow the use of ordinary
// functions as CredentialsProvider.
type CredentialsProviderFunc func(ctx context.Context, factor int, plugin string) (string, error)

// Credentials implements the CredentialsProvider interface.
func (f CredentialsProviderFunc) Credentials(ctx context.Context, factor int, plugin string) (string, error) {
	return f(ctx, factor, plugin)
}

// server pub keys registry
var (
	serverPubKeyLock     sync.RWMutex
	serverPubKeyRegistry map[string]*rsa.PublicKey
)

// RegisterServerPubKey registers a server RSA public key which can be used to
// send data in a secure manner to the server without receiving the public key
// in a potentially insecure way from the server first.
// Registered keys can afterwards be used adding serverPubKey=<name> to the DSN.
//
// Note: The provided rsa.PublicKey instance is exclusively owned by the driver
// after registering it and may not be modified.
//
//	data, err := os.ReadFile("mykey.pem")
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	block, _ := pem.Decode(data)
//	if block == nil || block.Type != "PUBLIC KEY" {
//		log.Fatal("failed to decode PEM block containing public key")
//	}
//
//	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
//	if err != nil {
//		log.Fatal(err)
//	}
//
//	if rsaPubKey, ok := pub.(*rsa.PublicKey); ok {
//		mysql.RegisterServerPubKey("mykey", rsaPubKey)
//	} else {
//		log.Fatal("not a RSA public key")
//	}
func RegisterServerPubKey(name string, pubKey *rsa.PublicKey) {
	serverPubKeyLock.Lock()
	if serverPubKeyRegistry == nil {
		serverPubKeyRegistry = make(map[string]*rsa.PublicKey)
	}

	serverPubKeyRegistry[name] = pubKey
	serverPubKeyLock.Unlock()
}

// DeregisterServerPubKey removes the public key registered with the given name.
func DeregisterServerPubKey(name string) {
	serverPubKeyLock.Lock()
	if serverPubKeyRegistry != nil {
		delete(serverPubKeyRegistry, name)
	}
	serverPubKeyLock.Unlock()
}

func getServerPubKey(name string) (pubKey *rsa.PublicKey) {
	serverPubKeyLock.RLock()
	if v, ok := serverPubKeyRegistry[name]; ok {
		pubKey = v
	}
	serverPubKeyLock.RUnlock()
	return
}

// Hash password using pre 4.1 (old password) method
// https://github.com/atcurtis/mariadb/blob/master/mysys/my_rnd.c
type myRnd struct {
	seed1, seed2 uint32
}

const myRndMaxVal = 0x3FFFFFFF

// Pseudo random number generator
func newMyRnd(seed1, seed2 uint32) *myRnd {
	return &myRnd{
		seed1: seed1 % myRndMaxVal,
		seed2: seed2 % myRndMaxVal,
	}
}

// Tested to be equivalent to MariaDB's floating point variant
// http://play.golang.org/p/QHvhd4qved
// http://play.golang.org/p/RG0q4ElWDx
func (r *myRnd) NextByte() byte {
	r.seed1 = (r.seed1*3 + r.seed2) % myRndMaxVal
	r.seed2 = (r.seed1 + r.seed2 + 33) % myRndMaxVal

	return byte(uint64(r.seed1) * 31 / myRndMaxVal)
}

// Generate binary hash from byte string using insecure pre 4.1 method
func pwHash(password []byte) (result [2]uint32) {
	var add uint32 = 7
	var tmp uint32

	result[0] = 1345345333
	result[1] = 0x12345671

	for _, c := range password {
		// skip spaces and tabs in password
		if c == ' ' || c == '\t' {
			continue
		}

		tmp = uint32(c)
		result[0] ^= (((result[0] & 63) + add) * tmp) + (result[0] << 8)
		result[1] += (result[1] << 8) ^ result[0]
		add += tmp
	}

	// Remove sign bit (1<<31)-1)
	result[0] &= 0x7FFFFFFF
	result[1] &= 0x7FFFFFFF

	return
}

// Hash password using insecure pre 4.1 method
func scrambleOldPassword(scramble []byte, password string) []byte {
	scramble = scramble[:8]

	hashPw := pwHash([]byte(password))
	hashSc := pwHash(scramble)

	r := newMyRnd(hashPw[0]^hashSc[0], hashPw[1]^hashSc[1])

	var out [8]byte
	for i := range out {
		out[i] = r.NextByte() + 64
	}

	mask := r.NextByte()
	for i := range out {
		out[i] ^= mask
	}

	return out[:]
}

// Hash password using 4.1+ method (SHA1)
func scramblePassword(scramble []byte, password string) []byte {
	if len(password) == 0 {
		return nil
	}

	// stage1Hash = SHA1(password)
	crypt := sha1.New()
	crypt.Write([]byte(password))
	stage1 := crypt.Sum(nil)

	// scrambleHash = SHA1(scramble + SHA1(stage1Hash))
	// inner Hash
	crypt.Reset()
	crypt.Write(stage1)
	hash := crypt.Sum(nil)

	// outer Hash
	crypt.Reset()
	crypt.Write(scramble)
	crypt.Write(hash)
	scramble = crypt.Sum(nil)

	// token = scrambleHash XOR stage1Hash
	for i := range scramble {
		scramble[i] ^= stage1[i]
	}
	return scramble
}

// Hash password using MySQL 8+ method (SHA256)
func scrambleSHA256Password(scramble []byte, password string) []byte {
	if len(password) == 0 {
		return nil
	}

	// XOR(SHA256(password), SHA256(SHA256(SHA256(password)), scramble))

	crypt := sha256.New()
	crypt.Write([]byte(password))
	message1 := crypt.Sum(nil)

	crypt.Reset()
	crypt.Write(message1)
	message1Hash := crypt.Sum(nil)

	crypt.Reset()
	crypt.Write(message1Hash)
	crypt.Write(scramble)
	message2 := crypt.Sum(nil)

	for i := range message1 {
		message1[i] ^= message2[i]
	}

	return message1
}

func encryptPassword(password string, seed []byte, pub *rsa.PublicKey) ([]byte, error) {
	plain := make([]byte, len(password)+1)
	copy(plain, password)
	for i := range plain {
		j := i % len(seed)
		plain[i] ^= seed[j]
	}
	sha1 := sha1.New()
	return rsa.EncryptOAEP(sha1, rand.Reader, pub, plain, nil)
}

// authEd25519 does ed25519 authentication used by MariaDB.
func authEd25519(scramble []byte, password string) ([]byte, error) {
	// Derived from https://github.com/MariaDB/server/blob/d8e6bb00888b1f82c031938f4c8ac5d97f6874c3/plugin/auth_ed25519/ref10/sign.c
	// Code style is from https://cs.opensource.google/go/go/+/refs/tags/go1.21.5:src/crypto/ed25519/ed25519.go;l=207
	h := sha512.Sum512([]byte(password))

	s, err := edwards25519.NewScalar().SetBytesWithClamping(h[:32])
	if err != nil {
		return nil, err
	}
	A := (&edwards25519.Point{}).ScalarBaseMult(s)

	mh := sha512.New()
	mh.Write(h[32:])
	mh.Write(scramble)
	messageDigest := mh.Sum(nil)
	r, err := edwards25519.NewScalar().SetUniformBytes(messageDigest)
	if err != nil {
		return nil, err
	}

	R := (&edwards25519.Point{}).ScalarBaseMult(r)

	kh := sha512.New()
	kh.Write(R.Bytes())
	kh.Write(A.Bytes())
	kh.Write(scramble)
	hramDigest := kh.Sum(nil)
	k, err := edwards25519.NewScalar().SetUniformBytes(hramDigest)
	if err != nil {
		return nil, err
	}

	S := k.MultiplyAdd(k, s, r)

	return append(R.Bytes(), S.Bytes()...), nil
}

// factorCredentials returns the authentication material for the given 1-based
// authentication factor and plugin.
//
// When a CredentialsProvider is configured it is consulted for every factor.
// Otherwise only the first factor is covered by Config.Passwd; it is never
// reused for additional factors.
func (mc *mysqlConn) factorCredentials(ctx context.Context, factor int, plugin string) (string, error) {
	if mc.cfg.credentialsProvider != nil {
		return mc.cfg.credentialsProvider.Credentials(ctx, factor, plugin)
	}
	if factor == 1 {
		return mc.cfg.Passwd, nil
	}
	return "", fmt.Errorf("%w: factor %d, plugin %q", ErrNoCredentials, factor, plugin)
}

// authForFactor resolves the credentials for the given factor and computes
// the authentication response for the requested plugin.
func (mc *mysqlConn) authForFactor(ctx context.Context, factor int, authData []byte, plugin string) ([]byte, error) {
	passwd, err := mc.factorCredentials(ctx, factor, plugin)
	if err != nil {
		return nil, err
	}
	return mc.authWithPassword(authData, plugin, passwd)
}

func (mc *mysqlConn) sendEncryptedPassword(seed []byte, pub *rsa.PublicKey, passwd string) error {
	enc, err := encryptPassword(passwd, seed, pub)
	if err != nil {
		return err
	}
	return mc.writeAuthSwitchPacket(enc)
}

func (mc *mysqlConn) auth(authData []byte, plugin string) ([]byte, error) {
	return mc.authWithPassword(authData, plugin, mc.cfg.Passwd)
}

func (mc *mysqlConn) authWithPassword(authData []byte, plugin, passwd string) ([]byte, error) {
	switch plugin {
	case "caching_sha2_password":
		authResp := scrambleSHA256Password(authData, passwd)
		return authResp, nil

	case "mysql_old_password":
		if !mc.cfg.AllowOldPasswords {
			return nil, ErrOldPassword
		}
		if len(passwd) == 0 {
			return nil, nil
		}
		// Note: there are edge cases where this should work but doesn't;
		// this is currently "wontfix":
		// https://github.com/go-sql-driver/mysql/issues/184
		authResp := append(scrambleOldPassword(authData[:8], passwd), 0)
		return authResp, nil

	case "mysql_clear_password":
		if !mc.cfg.AllowCleartextPasswords {
			return nil, ErrCleartextPassword
		}
		// http://dev.mysql.com/doc/refman/5.7/en/cleartext-authentication-plugin.html
		// http://dev.mysql.com/doc/refman/5.7/en/pam-authentication-plugin.html
		return append([]byte(passwd), 0), nil

	case "mysql_native_password":
		if !mc.cfg.AllowNativePasswords {
			return nil, ErrNativePassword
		}
		// https://dev.mysql.com/doc/dev/mysql-server/8.4.5/page_protocol_connection_phase_authentication_methods_native_password_authentication.html
		// Native password authentication only need and will need 20-byte challenge.
		authResp := scramblePassword(authData[:20], passwd)
		return authResp, nil

	case "sha256_password":
		if len(passwd) == 0 {
			return []byte{0}, nil
		}
		// unlike caching_sha2_password, sha256_password does not accept
		// cleartext password on unix transport.
		if mc.cfg.TLS != nil {
			// write cleartext auth packet
			return append([]byte(passwd), 0), nil
		}

		pubKey := mc.cfg.pubKey
		if pubKey == nil {
			// request public key from server
			return []byte{1}, nil
		}

		// encrypted password
		enc, err := encryptPassword(passwd, authData, pubKey)
		return enc, err

	case "client_ed25519":
		if len(authData) != 32 {
			return nil, ErrMalformPkt
		}
		return authEd25519(authData, passwd)

	default:
		mc.log("unknown auth plugin:", plugin)
		return nil, ErrUnknownPlugin
	}
}

// handleAuthResult drives the authentication exchange after the initial
// handshake response has been sent. It handles plugin switches
// (AuthSwitchRequest), multi-factor authentication (AuthNextFactor) and
// plugin-specific continuations (AuthMoreData), and only returns once the
// server has accepted or rejected the whole authentication flow.
func (mc *mysqlConn) handleAuthResult(ctx context.Context, oldAuthData []byte, plugin string) error {
	factor := 1
	authData := oldAuthData

	for {
		data, err := mc.readPacket()
		if err != nil {
			return err
		}

		switch data[0] {
		case iOK:
			// resultUnchanged, since auth happens before any queries or
			// commands have been executed.
			return mc.resultUnchanged().handleOkPacket(data)

		case iERR:
			return mc.handleErrorPacket(data)

		case iEOF, iAuthNextFactor:
			// Auth Switch Request or Auth Next Factor Request.
			// https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_auth_switch_request.html
			// https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_auth_next_factor_request.html
			if data[0] == iAuthNextFactor {
				// The server requests authentication material for the next factor.
				factor++
			}

			if len(data) == 1 {
				if data[0] != iEOF {
					return ErrMalformPkt
				}
				// https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_connection_phase_packets_protocol_old_auth_switch_request.html
				// If CLIENT_PLUGIN_AUTH capability is not supported, no new
				// cipher is sent and we have to keep using the cipher sent in
				// the init packet.
				plugin = "mysql_old_password"
			} else {
				pluginEndIndex := bytes.IndexByte(data, 0x00)
				if pluginEndIndex < 0 {
					return ErrMalformPkt
				}
				plugin = string(data[1:pluginEndIndex])
				newAuthData := data[pluginEndIndex+1:]
				if len(newAuthData) > 0 && newAuthData[len(newAuthData)-1] == 0 {
					newAuthData = newAuthData[:len(newAuthData)-1]
				}
				// copy data from read buffer to owned slice
				authData = bytes.Clone(newAuthData)
			}

			authResp, err := mc.authForFactor(ctx, factor, authData, plugin)
			if err != nil {
				return err
			}
			if err := mc.writeAuthSwitchPacket(authResp); err != nil {
				return err
			}

		case iAuthMoreData:
			if err := mc.handleAuthMoreData(ctx, factor, authData, plugin, data[1:]); err != nil {
				return err
			}

		default:
			return ErrMalformPkt
		}
	}
}

// handleAuthMoreData performs the plugin-specific continuation of the
// authentication exchange for AuthMoreData packets. After it returns, the
// server sends the next packet of the authentication flow (OK, ERR or a
// request for the next factor).
func (mc *mysqlConn) handleAuthMoreData(ctx context.Context, factor int, authData []byte, plugin string, data []byte) error {
	switch plugin {
	// https://dev.mysql.com/blog-archive/preparing-your-community-connector-for-mysql-8-part-2-sha256/
	case "caching_sha2_password":
		if len(data) != 1 {
			return ErrMalformPkt
		}
		switch data[0] {
		case cachingSha2PasswordFastAuthSuccess:
			// The final OK packet (or a request for the next factor) follows.
			return nil

		case cachingSha2PasswordPerformFullAuthentication:
			passwd, err := mc.factorCredentials(ctx, factor, plugin)
			if err != nil {
				return err
			}
			if mc.cfg.TLS != nil || mc.cfg.Net == "unix" {
				// write cleartext auth packet
				return mc.writeAuthSwitchPacket(append([]byte(passwd), 0))
			}

			pubKey := mc.cfg.pubKey
			if pubKey == nil {
				// request public key from server
				pkt, err := mc.buf.takeSmallBuffer(4 + 1)
				if err != nil {
					return err
				}
				pkt[4] = cachingSha2PasswordRequestPublicKey
				if err = mc.writePacket(pkt); err != nil {
					return err
				}

				if pkt, err = mc.readPacket(); err != nil {
					return err
				} else if pkt[0] != iAuthMoreData {
					return fmt.Errorf("unexpected resp from server for caching_sha2_password, perform full authentication")
				}

				// parse public key
				block, rest := pem.Decode(pkt[1:])
				if block == nil {
					return fmt.Errorf("no pem data found, data: %s", rest)
				}
				pkix, err := x509.ParsePKIXPublicKey(block.Bytes)
				if err != nil {
					return err
				}
				pubKey = pkix.(*rsa.PublicKey)
			}

			// send encrypted password
			return mc.sendEncryptedPassword(authData, pubKey, passwd)

		default:
			return ErrMalformPkt
		}

	case "sha256_password":
		block, _ := pem.Decode(data)
		if block == nil {
			return fmt.Errorf("no Pem data found, data: %s", data)
		}

		pub, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return err
		}

		passwd, err := mc.factorCredentials(ctx, factor, plugin)
		if err != nil {
			return err
		}

		// send encrypted password
		return mc.sendEncryptedPassword(authData, pub.(*rsa.PublicKey), passwd)

	default:
		mc.log("unexpected auth more data for plugin:", plugin)
		return ErrUnknownPlugin
	}
}
