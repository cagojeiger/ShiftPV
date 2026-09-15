package certificate

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
)

type material struct {
	caPEM      []byte
	caKeyPEM   []byte
	certPEM    []byte
	certKeyPEM []byte
	ca         *x509.Certificate
	caKey      crypto.Signer
	cert       *tls.Certificate
}

func generateMaterial(now time.Time, config Config, dnsNames []string) (material, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return material{}, fmt.Errorf("generate webhook CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return material{}, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "shiftpv-webhook-ca"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(config.CAValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, caKey.Public(), caKey)
	if err != nil {
		return material{}, fmt.Errorf("create webhook CA certificate: %w", err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return material{}, fmt.Errorf("parse generated webhook CA certificate: %w", err)
	}
	caKeyDER, err := x509.MarshalPKCS8PrivateKey(caKey)
	if err != nil {
		return material{}, fmt.Errorf("encode webhook CA key: %w", err)
	}
	base := material{
		caPEM:    pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		caKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKeyDER}),
		ca:       ca,
		caKey:    caKey,
	}
	return renewServing(now, config, base, dnsNames)
}

func renewServing(now time.Time, config Config, current material, dnsNames []string) (material, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return material{}, fmt.Errorf("generate webhook serving key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return material{}, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: dnsNames[0]},
		DNSNames:     append([]string(nil), dnsNames...),
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(config.ServingValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, current.ca, key.Public(), current.caKey)
	if err != nil {
		return material{}, fmt.Errorf("create webhook serving certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return material{}, fmt.Errorf("encode webhook serving key: %w", err)
	}
	current.certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	current.certKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	pair, err := tls.X509KeyPair(current.certPEM, current.certKeyPEM)
	if err != nil {
		return material{}, fmt.Errorf("load generated webhook serving certificate: %w", err)
	}
	pair.Leaf, err = x509.ParseCertificate(certDER)
	if err != nil {
		return material{}, fmt.Errorf("parse generated webhook serving certificate: %w", err)
	}
	current.cert = &pair
	return current, nil
}

func parseMaterial(secret *corev1.Secret, dnsNames []string, now time.Time) (material, error) {
	if secret == nil {
		return material{}, fmt.Errorf("webhook TLS Secret does not exist")
	}
	caBlock, _ := pem.Decode(secret.Data[caCertificateKey])
	if caBlock == nil || caBlock.Type != "CERTIFICATE" {
		return material{}, fmt.Errorf("webhook CA certificate is missing")
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil || !ca.IsCA {
		return material{}, fmt.Errorf("webhook CA certificate is invalid")
	}
	caKey, err := parseSigner(secret.Data[caPrivateKeyKey])
	if err != nil {
		return material{}, err
	}
	if !publicKeysEqual(ca.PublicKey, caKey.Public()) {
		return material{}, fmt.Errorf("webhook CA key does not match certificate")
	}
	pair, err := tls.X509KeyPair(secret.Data[tlsCertificateKey], secret.Data[tlsPrivateKeyKey])
	if err != nil {
		return material{}, fmt.Errorf("load webhook serving certificate: %w", err)
	}
	if len(pair.Certificate) == 0 {
		return material{}, fmt.Errorf("webhook serving certificate chain is empty")
	}
	pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return material{}, fmt.Errorf("parse webhook serving certificate: %w", err)
	}
	if now.Before(pair.Leaf.NotBefore) || !now.Before(pair.Leaf.NotAfter) || now.Before(ca.NotBefore) || !now.Before(ca.NotAfter) {
		return material{}, fmt.Errorf("webhook certificate is outside its validity window")
	}
	if err := pair.Leaf.CheckSignatureFrom(ca); err != nil {
		return material{}, fmt.Errorf("verify webhook serving certificate issuer: %w", err)
	}
	for _, name := range dnsNames {
		if err := pair.Leaf.VerifyHostname(name); err != nil {
			return material{}, fmt.Errorf("verify webhook serving DNS name %q: %w", name, err)
		}
	}
	return material{
		caPEM:      append([]byte(nil), secret.Data[caCertificateKey]...),
		caKeyPEM:   append([]byte(nil), secret.Data[caPrivateKeyKey]...),
		certPEM:    append([]byte(nil), secret.Data[tlsCertificateKey]...),
		certKeyPEM: append([]byte(nil), secret.Data[tlsPrivateKeyKey]...),
		ca:         ca,
		caKey:      caKey,
		cert:       &pair,
	}, nil
}

func parseSigner(value []byte) (crypto.Signer, error) {
	block, _ := pem.Decode(value)
	if block == nil {
		return nil, fmt.Errorf("webhook CA private key is missing")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse webhook CA private key: %w", err)
	}
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("webhook CA private key cannot sign certificates")
	}
	return signer, nil
}

func publicKeysEqual(left, right crypto.PublicKey) bool {
	leftDER, leftErr := x509.MarshalPKIXPublicKey(left)
	rightDER, rightErr := x509.MarshalPKIXPublicKey(right)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(leftDER, rightDER)
}

func mergeCertificateBundles(values ...[]byte) []byte {
	seen := map[string]struct{}{}
	var result []byte
	for _, value := range values {
		remaining := value
		for len(remaining) > 0 {
			block, rest := pem.Decode(remaining)
			if block == nil {
				break
			}
			remaining = rest
			if block.Type != "CERTIFICATE" {
				continue
			}
			certificate, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				continue
			}
			key := string(certificate.Raw)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})...)
		}
	}
	return result
}

func transitionBaseCA(secretCA, trustedCA []byte, active *tls.Certificate) []byte {
	if certificate := firstCertificate(secretCA, nil); certificate != nil {
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	}
	var activeLeaf *x509.Certificate
	if active != nil {
		activeLeaf = active.Leaf
		if activeLeaf == nil && len(active.Certificate) > 0 {
			activeLeaf, _ = x509.ParseCertificate(active.Certificate[0])
		}
	}
	certificate := firstCertificate(trustedCA, func(candidate *x509.Certificate) bool {
		return activeLeaf != nil && activeLeaf.CheckSignatureFrom(candidate) == nil
	})
	if certificate == nil {
		certificate = firstCertificate(trustedCA, nil)
	}
	if certificate == nil {
		return nil
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
}

func firstCertificate(bundle []byte, accept func(*x509.Certificate) bool) *x509.Certificate {
	remaining := bundle
	for len(remaining) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil {
			return nil
		}
		remaining = rest
		if block.Type != "CERTIFICATE" {
			continue
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err == nil && (accept == nil || accept(certificate)) {
			return certificate
		}
	}
	return nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("generate certificate serial: %w", err)
	}
	return serial, nil
}
