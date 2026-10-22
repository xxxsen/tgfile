package telegram

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestTLSCertificateTrust(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	certificate := server.Certificate()
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	options := x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: certificate.NotBefore.Add(time.Second),
	}

	t.Run("trusted certificate", func(t *testing.T) {
		chains, err := certificate.Verify(options)
		if err != nil || len(chains) != 1 {
			t.Fatalf("trusted certificate verification: chains=%d, err=%v", len(chains), err)
		}
	})

	t.Run("untrusted certificate", func(t *testing.T) {
		untrustedOptions := options
		untrustedOptions.Roots = x509.NewCertPool()
		_, err := certificate.Verify(untrustedOptions)
		var unknownAuthority x509.UnknownAuthorityError
		if !errors.As(err, &unknownAuthority) {
			t.Fatalf("untrusted certificate: expected UnknownAuthorityError, got %v", err)
		}
	})

	t.Run("expired certificate", func(t *testing.T) {
		expiredOptions := options
		expiredOptions.CurrentTime = certificate.NotAfter.Add(time.Second)
		_, err := certificate.Verify(expiredOptions)
		var invalidCertificate x509.CertificateInvalidError
		if !errors.As(err, &invalidCertificate) || invalidCertificate.Reason != x509.Expired {
			t.Fatalf("expired certificate: expected Expired, got %v", err)
		}
	})
}

func TestTLSCertificateTrustRejectsDistrustedIntermediate(t *testing.T) {
	t.Parallel()

	// Public EM OV TLS CA - G3C certificate from CCADB; no private key is needed.
	// Source: https://ccadb.my.salesforce-sites.com/mozilla/PublicIntermediateCertsRevokedWithPEMCSV
	// Its SPKI is denied by Go's CVE-2026-111943 fix, even with explicit trust.
	certificatePEM, err := os.ReadFile("testdata/distrusted_intermediate.pem")
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatal("expected exactly one public certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	spkiHash := sha256.Sum256(certificate.RawSubjectPublicKeyInfo)
	const expectedSPKIHash = "63ecefee431d49523bc1136c4e72d0206a477086caf606cd5cf025686c109cf8"
	if hex.EncodeToString(spkiHash[:]) != expectedSPKIHash {
		t.Fatal("fixture must retain the distrusted intermediate's public key")
	}

	// Explicit roots and a time within validity isolate the Go denylist from the
	// operating system trust store, certificate expiry, and external services.
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	chains, err := certificate.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: certificate.NotBefore.Add(time.Second),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})
	var invalidCertificate x509.CertificateInvalidError
	if !errors.As(err, &invalidCertificate) || invalidCertificate.Reason != x509.NoValidChains {
		t.Fatalf("distrusted intermediate: expected NoValidChains, got chains=%d, err=%v", len(chains), err)
	}
	if len(chains) != 0 {
		t.Fatalf("distrusted intermediate returned %d verified chains", len(chains))
	}
}
