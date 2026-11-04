package opcua

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"time"

	"github.com/gopcua/opcua/uapolicy"

	"github.com/getlaraplate/laraedge/internal/atomicfile"
)

// The PKI of the driver lives under <data_dir>/opcua/pki/: own/ holds the
// agent's application certificate and key, trusted/ the server certificates
// the operator accepted, rejected/ the ones a connect attempt met and refused.
// The library validates no server certificate, so trust is ours (O1).
const (
	ownCertFile   = "cert.der"
	ownKeyFile    = "key.pem"
	ownRSABits    = 2048
	ownValidity   = 10 // years
	ownClockSkew  = time.Hour
	ownTmpPattern = "own-*.tmp"
	// ownTmpStaleAge is the age after which a temporary own directory is a
	// leftover: a younger one may be a generation still running in another
	// input or process (`opcua trust`), which a sweep must not break.
	ownTmpStaleAge = time.Hour
	ownAppURIBase  = "urn:laraplate-edge:"
	certSuffix     = ".der"
)

// Errors of the own/ directory are fixed texts: they never carry file content.
var (
	errAgentIDRequired = errors.New("opcua pki: agent id is required")
	errOwnIncomplete   = errors.New("opcua pki: own certificate directory is incomplete (certificate or key missing)")
	errOwnCertInvalid  = errors.New("opcua pki: own certificate is not a valid certificate")
	errOwnKeyInvalid   = errors.New("opcua pki: own key is not a valid PKCS#8 RSA private key")
	errOwnMismatch     = errors.New("opcua pki: own certificate and key do not match")
	errCertInvalid     = errors.New("opcua pki: not a valid certificate")
)

// thumbprintName is a stored certificate file name: 40 lowercase hex characters
// (SHA-1 of the leaf DER) and the .der suffix.
var (
	thumbprintRE     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	thumbprintFileRE = regexp.MustCompile(`^[0-9a-f]{40}\.der$`)
	// ownTmpRE is the name os.MkdirTemp gives ownTmpPattern (a decimal number
	// between the prefix and the suffix), or that name claimed by a sweep
	// (ownTmpClaim) that may have crashed before removing it: group 1 is the
	// unclaimed name, group 2 the claim time (Unix seconds, 16 hex digits).
	ownTmpRE = regexp.MustCompile(`^(own-[0-9]+\.tmp)(?:\.sweep-([0-9a-f]{16})-[0-9a-f]{16})?$`)
)

// PKIDir is the directory holding own/, trusted/ and rejected/ for dataDir.
func PKIDir(dataDir string) string {
	return filepath.Join(dataDir, "opcua", "pki")
}

// Thumbprint is the lowercase hex SHA-1 of the leaf certificate in der, the
// identity of a certificate in the trust store (40 characters).
func Thumbprint(der []byte) string {
	return hex.EncodeToString(uapolicy.Thumbprint(der))
}

// EnsureOwnCertificate returns the agent's application certificate (DER) and
// key, creating the pair under <dataDir>/opcua/pki/own/ when the directory
// does not exist (O2). The pair is written into a temporary sibling directory
// that is renamed to own/, so two processes never leave a mismatched pair; the
// loser of the rename reads the winner's pair. An own/ that exists but is
// incomplete or inconsistent is an error: it is never regenerated.
func EnsureOwnCertificate(dataDir, agentID string, now time.Time) ([]byte, *rsa.PrivateKey, error) {
	if agentID == "" {
		return nil, nil, errAgentIDRequired
	}

	pki := PKIDir(dataDir)
	own := filepath.Join(pki, "own")

	if cert, key, err := loadOwn(own); !errors.Is(err, os.ErrNotExist) {
		return cert, key, err
	}

	if err := os.MkdirAll(pki, 0o700); err != nil {
		return nil, nil, fmt.Errorf("opcua pki: create directory: %w", err)
	}

	certDER, key, keyPEM, err := generateOwn(agentID, now)
	if err != nil {
		return nil, nil, err
	}

	tmp, err := os.MkdirTemp(pki, ownTmpPattern)
	if err != nil {
		return nil, nil, fmt.Errorf("opcua pki: create temporary directory: %w", err)
	}

	renamed := false

	defer func() {
		if !renamed {
			_ = os.RemoveAll(tmp)
		}
	}()

	if err := atomicfile.Write(tmp, ownCertFile, certDER); err != nil {
		return nil, nil, err
	}

	if err := atomicfile.Write(tmp, ownKeyFile, keyPEM); err != nil {
		return nil, nil, err
	}

	if err := os.Rename(tmp, own); err != nil {
		// Another process or goroutine renamed first: use its pair.
		if cert, k, loadErr := loadOwn(own); loadErr == nil {
			return cert, k, nil
		} else if !errors.Is(loadErr, os.ErrNotExist) {
			return nil, nil, loadErr
		}

		return nil, nil, fmt.Errorf("opcua pki: install own certificate: %w", err)
	}

	renamed = true

	if err := atomicfile.SyncDir(pki); err != nil {
		return nil, nil, err
	}

	return certDER, key, nil
}

// ownTmpClaim is the infix of a directory a sweep claimed:
// own-<n>.tmp.sweep-<claim time>-<random>, both 16 hex digits.
const ownTmpClaim = ".sweep-"

// SweepStaleOwn removes the temporary own directories (own-<n>.tmp) that
// EnsureOwnCertificate leaves in the PKI directory of dataDir when the process
// dies between creating one and renaming it to own/: they can hold a private
// key. It removes only what EnsureOwnCertificate creates, or what an
// interrupted sweep claimed (own-<n>.tmp.sweep-<hex>): a direct entry of the
// PKI directory with that exact name that is a plain directory (isPlainDir:
// never a symlink, a junction or a mount point, which are not followed), last
// modified at least ownTmpStaleAge before now. A directory whose time is in
// the future (the clock was set back) waits until now passes it by that age.
// now is wall-clock time, as file times are.
//
// Sweeps may run together (inputs starting at once, the agent and a command).
// Each directory is first claimed by renaming it, in the same directory, to
// own-<n>.tmp.sweep-<random hex>: the rename is atomic, so one sweep wins and
// removes it; a sweep whose rename finds the name gone, or is refused it (on
// Windows another sweep may still hold a handle inside), skips it. The claimed
// path must still be the directory looked at (a plain directory, os.SameFile
// with the identity read before the rename): one swapped in meanwhile is
// refused and left in place; one already gone was removed by someone else and
// is skipped. It is removed with
// os.RemoveAll on the claimed path, which unlinks a top-level symlink without
// following it and never follows one below. A sweep that dies between its
// claim and the removal leaves a claimed name, which the next sweep handles
// with the same checks (a rename keeps the directory's time) once the claim,
// whose time the name carries, is ownTmpStaleAge old. It returns, in
// name order and by their own-<n>.tmp name, the directories this call claimed
// and removed. A missing PKI directory is nothing to do.
func SweepStaleOwn(dataDir string, now time.Time) ([]string, error) {
	return sweepStaleOwnWith(dataDir, now, os.Rename)
}

// sweepStaleOwnWith is SweepStaleOwn with the rename of the claim; tests
// replace it to change the directory between the claim and the removal.
func sweepStaleOwnWith(dataDir string, now time.Time, rename func(oldPath, newPath string) error) ([]string, error) {
	pki := PKIDir(dataDir)

	entries, err := os.ReadDir(pki)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("opcua pki: list %s: %w", pki, err)
	}

	var (
		removed []string
		errs    []error
	)

	for _, e := range entries {
		match := ownTmpRE.FindStringSubmatch(e.Name())
		if match == nil || !isPlainDir(e.Type()) {
			continue
		}

		path := filepath.Join(pki, e.Name())

		if match[2] != "" && !claimIsStale(match[2], now) {
			continue
		}

		info, err := os.Lstat(path)
		if err != nil || !isPlainDir(info.Mode()) || now.Sub(info.ModTime()) < ownTmpStaleAge {
			continue
		}

		// Load the identity of the directory while it still has this name:
		// on Windows Lstat leaves the file id unread and os.SameFile reads it
		// lazily by opening the saved path, which the claim renames away.
		// On Unix the identity is already in info and this is always true.
		if !os.SameFile(info, info) {
			continue
		}

		claimed, ok, err := claimOwnTmp(pki, path, match[1], now, rename)
		if err != nil {
			errs = append(errs, fmt.Errorf("opcua pki: claim %s: %w", path, err))

			continue
		}

		if !ok {
			continue
		}

		claimedInfo, err := os.Lstat(claimed)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}

		if err != nil || !isPlainDir(claimedInfo.Mode()) || !os.SameFile(info, claimedInfo) {
			errs = append(errs, fmt.Errorf("opcua pki: %s changed while it was claimed; left in place", claimed))

			continue
		}

		if err := os.RemoveAll(claimed); err != nil {
			errs = append(errs, fmt.Errorf("opcua pki: remove %s: %w", claimed, err))

			continue
		}

		removed = append(removed, filepath.Join(pki, match[1]))
	}

	slices.Sort(removed)

	return removed, errors.Join(errs...)
}

// claimIsStale reports whether the claim time of a claimed name (16 hex
// digits of Unix seconds) is at least ownTmpStaleAge before now: a younger
// claim belongs to a sweep that may still be removing it, and re-claiming it
// would let both report it. The comparison stays in seconds, so a claim time
// beyond any clock is never stale (time.Unix would wrap it into the past).
func claimIsStale(claimedAt string, now time.Time) bool {
	seconds, err := strconv.ParseUint(claimedAt, 16, 64)
	if err != nil {
		return false
	}

	cutoff := now.Add(-ownTmpStaleAge).Unix()

	return cutoff >= 0 && seconds <= uint64(cutoff)
}

// claimOwnTmp renames path with rename to
// <pki>/<base>.sweep-<now in Unix seconds>-<random>, both 16 hex digits, and
// returns the new path; ok is false, with no error, when path is gone or the
// rename is refused for permission (another sweep claimed it first, or on
// Windows still holds a handle inside it).
func claimOwnTmp(pki, path, base string, now time.Time, rename func(oldPath, newPath string) error) (claimed string, ok bool, err error) {
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", false, err
	}

	claimed = filepath.Join(pki, fmt.Sprintf("%s%s%016x-%s", base, ownTmpClaim, uint64(max(now.Unix(), 0)), hex.EncodeToString(suffix[:])))

	if err := rename(path, claimed); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
			return "", false, nil
		}

		return "", false, err
	}

	return claimed, true, nil
}

// isPlainDir reports whether mode is a directory and nothing else: not a
// symlink, and not irregular (on Windows a junction or a mount point reports
// ModeDir with ModeIrregular).
func isPlainDir(mode os.FileMode) bool {
	return mode.Type() == os.ModeDir
}

// generateOwn builds a new certificate and key; the PEM is the PKCS#8 encoding
// of the key.
func generateOwn(agentID string, now time.Time) (certDER []byte, key *rsa.PrivateKey, keyPEM []byte, err error) {
	appURI, err := url.Parse(ownAppURIBase + agentID)
	if err != nil {
		return nil, nil, nil, errors.New("opcua pki: agent id cannot form an application URI")
	}

	key, err = rsa.GenerateKey(rand.Reader, ownRSABits)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opcua pki: generate key: %w", err)
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opcua pki: generate serial: %w", err)
	}

	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "laraplate-edge " + agentID,
			Organization: []string{"Laraplate"},
		},
		NotBefore: now.Add(-ownClockSkew),
		NotAfter:  now.AddDate(ownValidity, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment |
			x509.KeyUsageKeyEncipherment | x509.KeyUsageDataEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		SignatureAlgorithm:    x509.SHA256WithRSA,
		URIs:                  []*url.URL{appURI},
	}
	if host, hostErr := os.Hostname(); hostErr == nil && host != "" {
		tpl.DNSNames = []string{host}
	}

	certDER, err = x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opcua pki: create certificate: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("opcua pki: encode key: %w", err)
	}

	return certDER, key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8}), nil
}

// loadOwn reads the pair in own. It returns an error wrapping os.ErrNotExist
// only when the directory itself is missing; a directory without both files,
// or with files that do not parse or do not match, is a damaged pair.
func loadOwn(own string) ([]byte, *rsa.PrivateKey, error) {
	if _, err := os.Stat(own); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, os.ErrNotExist
		}

		return nil, nil, fmt.Errorf("opcua pki: inspect own certificate directory: %w", err)
	}

	keyPath := filepath.Join(own, ownKeyFile)
	certDER, certErr := os.ReadFile(filepath.Join(own, ownCertFile))
	keyPEM, keyErr := os.ReadFile(keyPath)

	if errors.Is(certErr, os.ErrNotExist) || errors.Is(keyErr, os.ErrNotExist) {
		return nil, nil, errOwnIncomplete
	}

	if err := checkKeyMode(keyPath, runtime.GOOS); err != nil {
		return nil, nil, err
	}

	if certErr != nil || keyErr != nil {
		return nil, nil, errors.New("opcua pki: read own certificate files failed")
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, errOwnCertInvalid
	}

	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, nil, errOwnKeyInvalid
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, errOwnKeyInvalid
	}

	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, nil, errOwnKeyInvalid
	}

	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, nil, errOwnMismatch
	}

	return certDER, key, nil
}

// checkKeyMode refuses, on a system with POSIX file modes, a key file whose
// mode grants anything beyond read and write to its owner (EnsureOwnCertificate
// writes it 0600): another user may have read the key. The error names the path
// and the mode, never the content; the operator fixes the mode or removes own/.
func checkKeyMode(path, goos string) error {
	if goos == "windows" {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil {
		return errors.New("opcua pki: read own certificate files failed")
	}

	if mode := info.Mode().Perm(); mode&^0o600 != 0 {
		return fmt.Errorf("opcua pki: %s has mode %04o, want 0600 or narrower (chmod 600 it)", path, mode)
	}

	return nil
}

// CertInfo describes a certificate of the trust store.
type CertInfo struct {
	Thumbprint string
	Subject    string
	NotAfter   time.Time
}

// TrustStore is the set of server certificates the operator accepted (O1).
// Dir is the pki directory (PKIDir): the store reads and writes its trusted/
// and rejected/ subdirectories. Expiry is reported, never enforced: the
// operator's explicit trust decides.
type TrustStore struct {
	Dir string
}

// Trusted reports whether a certificate with this thumbprint (40 lowercase
// hex characters) is in trusted/: a regular file of that name whose content
// has that thumbprint. A file whose content does not match its name is not
// trusted and is left in place (List reports it). Anything else is not
// trusted.
func (s TrustStore) Trusted(thumb string) bool {
	if !thumbprintRE.MatchString(thumb) {
		return false
	}

	path := filepath.Join(s.Dir, "trusted", thumb+certSuffix)

	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}

	der, err := os.ReadFile(path)

	return err == nil && Thumbprint(der) == thumb
}

// Trust stores the leaf certificate of der in trusted/ and returns its
// thumbprint. Trusting a certificate twice is not an error.
func (s TrustStore) Trust(der []byte) (string, error) {
	return s.store("trusted", der)
}

// Reject stores the leaf certificate of der in rejected/, where the operator
// finds it to trust it later. It does not trust it.
func (s TrustStore) Reject(der []byte) error {
	_, err := s.store("rejected", der)

	return err
}

func (s TrustStore) store(sub string, der []byte) (string, error) {
	leaf, err := uapolicy.ParseCertificate(der)
	if err != nil {
		return "", errCertInvalid
	}

	dir := filepath.Join(s.Dir, sub)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("opcua pki: create %s directory: %w", sub, err)
	}

	thumb := Thumbprint(leaf.Raw)
	if err := atomicfile.Write(dir, thumb+certSuffix, leaf.Raw); err != nil {
		return "", err
	}

	return thumb, nil
}

// List describes the certificates in trusted/, ordered by thumbprint. Files
// whose name is not <40 hex>.der are ignored. A file that does not parse, or
// whose name is not its thumbprint, is reported in the returned error by file
// name only; the other certificates are still returned.
func (s TrustStore) List() ([]CertInfo, error) {
	dir := filepath.Join(s.Dir, "trusted")

	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("opcua pki: list trusted certificates: %w", err)
	}

	var (
		infos []CertInfo
		errs  []error
	)

	for _, e := range entries {
		name := e.Name()
		if !e.Type().IsRegular() || !thumbprintFileRE.MatchString(name) {
			continue
		}

		der, readErr := os.ReadFile(filepath.Join(dir, name))
		if readErr != nil {
			errs = append(errs, fmt.Errorf("opcua pki: trusted certificate %s cannot be read", name))

			continue
		}

		cert, parseErr := x509.ParseCertificate(der)
		if parseErr != nil {
			errs = append(errs, fmt.Errorf("opcua pki: trusted certificate %s is not a valid certificate", name))

			continue
		}

		thumb := name[:len(name)-len(certSuffix)]
		if Thumbprint(cert.Raw) != thumb {
			errs = append(errs, fmt.Errorf("opcua pki: trusted certificate %s does not match its name", name))

			continue
		}

		infos = append(infos, CertInfo{Thumbprint: thumb, Subject: cert.Subject.String(), NotAfter: cert.NotAfter})
	}

	return infos, errors.Join(errs...)
}

// ServerCertificate is the certificate a server presents for a security
// policy and mode, as the operator is shown it before trusting it.
type ServerCertificate struct {
	// DER is the certificate as the server sent it (a chain keeps its
	// leaf first).
	DER []byte
	// Thumbprint is the lowercase hex SHA-1 of the leaf, 40 characters.
	Thumbprint string
	// Subject is the common name, or the whole subject when there is none;
	// it is printable (control characters replaced).
	Subject  string
	NotAfter time.Time
}

// DiscoverServerCertificate asks the server of conn for its endpoints over a
// None channel and returns the certificate of the endpoint matching the
// policy and mode (ruling R1). It opens no session and checks no trust: the
// answer is not authenticated, so it only tells the operator what to decide
// on. The errors never carry the endpoint.
func DiscoverServerCertificate(ctx context.Context, conn Connection) (ServerCertificate, error) {
	ep, err := discover(ctx, conn)
	if err != nil {
		return ServerCertificate{}, err
	}

	if len(ep.ServerCertificate) == 0 {
		return ServerCertificate{}, fmt.Errorf("opcua discover endpoints: %w", errNoServerCertificate)
	}

	leaf, err := uapolicy.ParseCertificate(ep.ServerCertificate)
	if err != nil {
		return ServerCertificate{}, fmt.Errorf("opcua discover endpoints: %w", errCertInvalid)
	}

	subject := leaf.Subject.CommonName
	if subject == "" {
		subject = leaf.Subject.String()
	}

	return ServerCertificate{
		DER:        ep.ServerCertificate,
		Thumbprint: Thumbprint(ep.ServerCertificate),
		Subject:    Printable(subject),
		NotAfter:   leaf.NotAfter,
	}, nil
}
