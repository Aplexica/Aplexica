// Command releasekeycheck validates and fingerprints the public trust anchor
// used for AWS KMS release signatures. It never loads private key material and
// performs no signing operation.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
)

const maxPublicKeySize = 16 << 10

type fingerprints struct {
	PEMSHA256        string
	SPKISHA256Base64 string
}

func main() {
	filename := flag.String("public-key", "aplexica-release.pub", "PKIX public key PEM")
	flag.Parse()
	if flag.NArg() != 0 {
		fatalf("unexpected positional arguments")
	}
	result, err := check(*filename)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Printf("pem-sha256 %s\n", result.PEMSHA256)
	fmt.Printf("spki-sha256-base64 %s\n", result.SPKISHA256Base64)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "release public key: "+format+"\n", args...)
	os.Exit(1)
}

func check(filename string) (fingerprints, error) {
	info, err := os.Stat(filename)
	if err != nil {
		return fingerprints{}, err
	}
	if !info.Mode().IsRegular() {
		return fingerprints{}, errors.New("trust anchor must be a regular file")
	}
	if info.Size() == 0 || info.Size() > maxPublicKeySize {
		return fingerprints{}, fmt.Errorf("trust anchor size must be between 1 and %d bytes", maxPublicKeySize)
	}
	contents, err := os.ReadFile(filename)
	if err != nil {
		return fingerprints{}, err
	}
	block, rest := pem.Decode(contents)
	if block == nil || block.Type != "PUBLIC KEY" || len(block.Headers) != 0 {
		return fingerprints{}, errors.New("trust anchor must contain one unadorned PUBLIC KEY PEM block")
	}
	if len(bytes.TrimSpace(rest)) != 0 {
		return fingerprints{}, errors.New("trust anchor has trailing data or multiple PEM blocks")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return fingerprints{}, fmt.Errorf("parse PKIX public key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return fingerprints{}, errors.New("trust anchor must be an ECC_NIST_P256 public key")
	}
	pemDigest := sha256.Sum256(contents)
	spkiDigest := sha256.Sum256(block.Bytes)
	return fingerprints{
		PEMSHA256:        hex.EncodeToString(pemDigest[:]),
		SPKISHA256Base64: base64.StdEncoding.EncodeToString(spkiDigest[:]),
	}, nil
}
