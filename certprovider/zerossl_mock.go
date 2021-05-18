package certprovider

import (
	"bytes"
)

// MockZeroSSLProvider is a mock implementation of the CertProvider interface.
type MockZeroSSLProvider struct {
	Certs map[string]ZeroSSLExternalCert
}

func (z MockZeroSSLProvider) GenerateCsr(keyBytes []byte, domain string) (bytes.Buffer, error) {
	return *bytes.NewBuffer(keyBytes), nil
}

func (z MockZeroSSLProvider) RequestCert(csr bytes.Buffer, domain string) (ZeroSSLExternalCert, error) {
	return ZeroSSLExternalCert{}, nil
}

func (z MockZeroSSLProvider) ValidateCert(certificate ZeroSSLExternalCert) error {
	return nil
}

func (z *MockZeroSSLProvider) GetCert(certId string) (ZeroSSLExternalCert, error) {
	return z.Certs[certId], nil
}

func (z MockZeroSSLProvider) DownloadCert(certificate ZeroSSLExternalCert) (string, string, error) {
	return "", "", nil
}

func (z MockZeroSSLProvider) RevokeCert(certificateId string) error {
	return nil
}
