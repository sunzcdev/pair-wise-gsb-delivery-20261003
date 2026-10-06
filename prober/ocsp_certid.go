// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package prober

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"math/big"
	"time"
)

// The types below mirror the ASN.1 structure of an OCSP response (RFC 6960).
// They exist only to extract the CertID issuer name/key hashes, which
// ocsp.ParseResponseForCert does not surface. Fields we do not need are left
// untyped (asn1.RawValue).

type ocspResponseASN1 struct {
	Status   asn1.Enumerated
	Response ocspResponseBytesASN1 `asn1:"explicit,tag:0,optional"`
}

type ocspResponseBytesASN1 struct {
	ResponseType asn1.ObjectIdentifier
	Response     []byte
}

type ocspBasicResponseASN1 struct {
	TBSResponseData    ocspResponseDataASN1
	SignatureAlgorithm pkixAlgorithmIdentifier
	Signature          asn1.BitString
	Certs              []asn1.RawValue `asn1:"explicit,tag:0,optional"`
}

type ocspResponseDataASN1 struct {
	Version        int `asn1:"optional,default:0,explicit,tag:0"`
	RawResponderID asn1.RawValue
	ProducedAt     time.Time `asn1:"generalized"`
	Responses      []ocspSingleResponseASN1
}

type ocspSingleResponseASN1 struct {
	CertID ocspCertIDASN1
	// The cert status is a CHOICE (good/revoked/unknown); only CertID is used.
	Good             asn1.Flag       `asn1:"tag:0,optional"`
	Revoked          asn1.RawValue   `asn1:"tag:1,optional"`
	Unknown          asn1.Flag       `asn1:"tag:2,optional"`
	ThisUpdate       time.Time       `asn1:"generalized"`
	NextUpdate       asn1.RawValue   `asn1:"explicit,tag:0,optional"`
	SingleExtensions []asn1.RawValue `asn1:"explicit,tag:1,optional"`
}

type ocspCertIDASN1 struct {
	HashAlgorithm pkixAlgorithmIdentifier
	NameHash      []byte
	IssuerKeyHash []byte
	SerialNumber  *big.Int
}

// pkixAlgorithmIdentifier is the subset of pkix.AlgorithmIdentifier needed to
// select the hash function.
type pkixAlgorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

var idPKIXOCSPBasic = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 48, 1, 1}

// extractOCSPCertID locates the CertID for serial in a DER-encoded OCSP
// response.
func extractOCSPCertID(raw []byte, serial *big.Int) (*ocspCertIDASN1, error) {
	var response ocspResponseASN1
	if _, err := asn1.Unmarshal(raw, &response); err != nil {
		return nil, err
	}
	if !response.Response.ResponseType.Equal(idPKIXOCSPBasic) {
		return nil, errors.New("not a basic OCSP response")
	}

	var basic ocspBasicResponseASN1
	if _, err := asn1.Unmarshal(response.Response.Response, &basic); err != nil {
		return nil, err
	}

	data := basic.TBSResponseData

	for _, single := range data.Responses {
		if single.CertID.SerialNumber.Cmp(serial) == 0 {
			certID := single.CertID
			return &certID, nil
		}
	}
	return nil, errors.New("no CertID matching the certificate")
}

// verifyOCSPIssuer checks that the CertID in a stapled OCSP response hashes to
// the actual issuer: the issuer's subject name and the subjectPublicKey BIT
// STRING (per RFC 6960, matching how ocsp.CreateResponse computes them).
func verifyOCSPIssuer(raw []byte, cert, issuer *x509.Certificate, hash crypto.Hash) error {
	certID, err := extractOCSPCertID(raw, cert.SerialNumber)
	if err != nil {
		return err
	}

	if !hash.Available() {
		return errors.New("CertID hash algorithm is not available")
	}

	nameSum := hash.New()
	nameSum.Write(issuer.RawSubject)
	if !bytes.Equal(nameSum.Sum(nil), certID.NameHash) {
		return errors.New("CertID issuer name hash does not match the issuer certificate")
	}

	var publicKeyInfo struct {
		Algorithm pkixAlgorithmIdentifier
		PublicKey asn1.BitString
	}
	if _, err := asn1.Unmarshal(issuer.RawSubjectPublicKeyInfo, &publicKeyInfo); err != nil {
		return err
	}
	keySum := hash.New()
	keySum.Write(publicKeyInfo.PublicKey.RightAlign())
	if !bytes.Equal(keySum.Sum(nil), certID.IssuerKeyHash) {
		return errors.New("CertID issuer key hash does not match the issuer certificate")
	}
	return nil
}
