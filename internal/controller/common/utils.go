package common

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

func GetTLSConfig(secret *corev1.Secret) (*tls.Config, error) {
	caCert, ok := secret.Data["ca.pem"]
	if !ok {
		return nil, fmt.Errorf("secret missing 'ca.pem'")
	}
	certPem, ok := secret.Data["cert.pem"]
	if !ok {
		return nil, fmt.Errorf("secret missing 'cert.pem'")
	}
	keyPem, ok := secret.Data["key.pem"]
	if !ok {
		return nil, fmt.Errorf("secret missing 'key.pem'")
	}

	// Load CA
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("failed to append CA certificate")
	}

	// Load Client Cert/Key
	cert, err := tls.X509KeyPair(certPem, keyPem)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		RootCAs:      caCertPool,
		Certificates: []tls.Certificate{cert},
		// InsecureSkipVerify: true, // Optional: make configurable via DockerHostSpec?
	}, nil
}
