package config

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

// LoadRSAKeys reads the RSA key pair from disk and returns them parsed.
// Private key must be PKCS1 PEM; public key must be PKIX PEM (openssl rsa -pubout).
func (c *Config) LoadRSAKeys() (*rsa.PrivateKey, *rsa.PublicKey, error) {
	privPEM, err := os.ReadFile(c.JWTPrivateKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read private key %q: %w", c.JWTPrivateKeyPath, err)
	}
	block, _ := pem.Decode(privPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("decode private key PEM: empty block")
	}
	privateKey, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		// fallback: try PKCS8
		key, err2 := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err2 != nil {
			return nil, nil, fmt.Errorf("parse private key: %w", err)
		}
		var ok bool
		privateKey, ok = key.(*rsa.PrivateKey)
		if !ok {
			return nil, nil, fmt.Errorf("private key is not RSA")
		}
	}

	pubPEM, err := os.ReadFile(c.JWTPublicKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read public key %q: %w", c.JWTPublicKeyPath, err)
	}
	block, _ = pem.Decode(pubPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("decode public key PEM: empty block")
	}
	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse public key: %w", err)
	}
	publicKey, ok := pub.(*rsa.PublicKey)
	if !ok {
		return nil, nil, fmt.Errorf("public key is not RSA")
	}

	return privateKey, publicKey, nil
}
