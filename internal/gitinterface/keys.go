// SPDX-License-Identifier: Apache-2.0

package gitinterface

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/hiddeco/sshsig"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/gittuf/gittuf/internal/signerverifier"
	"github.com/gittuf/gittuf/internal/tuf"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	gitsignVerifier "github.com/sigstore/gitsign/pkg/git"
	gitsignRekor "github.com/sigstore/gitsign/pkg/rekor"
	"github.com/sigstore/sigstore/pkg/fulcioroots"
	"golang.org/x/crypto/ssh"
)

var (
	ErrSigningKeyNotSpecified     = errors.New("signing key not specified in git config")
	ErrUnknownSigningMethod       = errors.New("unknown signing method (not one of gpg, ssh, x509)")
	ErrUnableToSign               = errors.New("unable to sign Git object")
	ErrIncorrectVerificationKey   = errors.New("incorrect key provided to verify signature")
	ErrVerifyingSigstoreSignature = errors.New("unable to verify Sigstore signature")
	ErrVerifyingSSHSignature      = errors.New("unable to verify SSH signature")
	ErrInvalidSignature           = errors.New("unable to parse signature / signature has unexpected header")
)

type SigningMethod int

const (
	SigningMethodGPG SigningMethod = iota
	SigningMethodSSH
	SigningMethodX509
)

const (
	DefaultSigningProgramGPG  string = "gpg"
	DefaultSigningProgramSSH  string = "ssh-keygen"
	DefaultSigningProgramX509 string = "gpgsm"
)

const (
	namespaceSSHSignature      string = "git"
	gpgPrivateKeyPEMHeader     string = "PGP PRIVATE KEY"
	opensshPrivateKeyPEMHeader string = "OPENSSH PRIVATE KEY"
	rsaPrivateKeyPEMHeader     string = "RSA PRIVATE KEY"
	genericPrivateKeyPEMHeader string = "PRIVATE KEY"
)

func GetSigningCommand() (string, []string, error) {
	var args []string

	signingMethod, keyInfo, program, err := getSigningInfo()
	if err != nil {
		return "", nil, err
	}

	switch signingMethod {
	case SigningMethodGPG:
		if len(keyInfo) == 0 {
			args = []string{
				"-bsa", // b -> detach-sign, s -> sign, a -> armor
			}
		} else {
			args = []string{
				"-bsau", keyInfo, // b -> detach-sign, s -> sign, a -> armor, u -> local-user
			}
		}
	case SigningMethodSSH:
		if len(keyInfo) == 0 {
			return "", nil, ErrSigningKeyNotSpecified
		}
		args = []string{
			"-Y", "sign",
			"-n", "git", // Git namespace
			"-f", keyInfo,
		}
	case SigningMethodX509:
		if len(keyInfo) == 0 {
			args = []string{
				"-bsa", // b -> detach-sign, s -> sign, a -> armor
			}
		} else {
			args = []string{
				"-bsau", keyInfo, // b -> detach-sign, s -> sign, a -> armor, u -> local-user
			}
		}
	default:
		return "", nil, ErrUnknownSigningMethod
	}

	return program, args, nil
}

func getSigningInfo() (SigningMethod, string, string, error) {
	gitConfig, err := getConfig()
	if err != nil {
		return -1, "", "", err
	}

	signingMethod, err := getSigningMethod(gitConfig)
	if err != nil {
		return -1, "", "", err
	}

	keyInfo := getSigningKeyInfo(gitConfig)

	program := getSigningProgram(gitConfig, signingMethod)

	return signingMethod, keyInfo, program, nil
}

func getSigningMethod(gitConfig map[string]string) (SigningMethod, error) {
	format, ok := gitConfig["gpg.format"]
	if !ok {
		return SigningMethodGPG, nil
	}

	switch format {
	case "gpg":
		return SigningMethodGPG, nil
	case "ssh":
		return SigningMethodSSH, nil
	case "x509":
		return SigningMethodX509, nil
	}
	return -1, ErrUnknownSigningMethod
}

func getSigningKeyInfo(gitConfig map[string]string) string {
	keyInfo, ok := gitConfig["user.signingkey"]
	if !ok {
		return ""
	}
	return keyInfo
}

func getSigningProgram(gitConfig map[string]string, signingMethod SigningMethod) string {
	switch signingMethod {
	case SigningMethodSSH:
		program, ok := gitConfig["gpg.ssh.program"]
		if ok {
			return program
		}
		return DefaultSigningProgramSSH
	case SigningMethodX509:
		program, ok := gitConfig["gpg.x509.program"]
		if ok {
			return program
		}
		return DefaultSigningProgramX509
	}

	// Default to GPG
	program, ok := gitConfig["gpg.program"]
	if ok {
		return program
	}

	return DefaultSigningProgramGPG
}

// signGitObject signs a Git commit or tag using the user's configured Git
// config.
func signGitObject(contents []byte) (string, error) {
	command, args, err := GetSigningCommand()
	if err != nil {
		return "", err
	}

	cmd := exec.Command(command, args...)

	stdInWriter, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}

	stdOutReader, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	defer stdOutReader.Close()

	stdErrReader, err := cmd.StderrPipe()
	if err != nil {
		return "", err
	}
	defer stdErrReader.Close()

	if err = cmd.Start(); err != nil {
		return "", err
	}

	if _, err := stdInWriter.Write(contents); err != nil {
		return "", err
	}
	if err := stdInWriter.Close(); err != nil {
		return "", err
	}

	sig, err := io.ReadAll(stdOutReader)
	if err != nil {
		return "", err
	}

	e, err := io.ReadAll(stdErrReader)
	if err != nil {
		return "", err
	}

	if len(e) > 0 {
		fmt.Fprint(os.Stderr, string(e))
	}

	if err = cmd.Wait(); err != nil {
		return "", err
	}

	if len(sig) == 0 {
		return "", ErrUnableToSign
	}

	return string(sig), nil
}

func signGitObjectUsingKey(contents, pemKeyBytes []byte) (string, error) {
	block, _ := pem.Decode(pemKeyBytes)
	if block == nil {
		// openpgp implements its own armor-decode method, pem.Decode considers
		// the input invalid. We haven't tested if this is universal, so in case
		// pem.Decode does succeed on a GPG key, we catch it below.
		return signGitObjectUsingGPGKey(contents, pemKeyBytes)
	}

	switch block.Type {
	case gpgPrivateKeyPEMHeader:
		return signGitObjectUsingGPGKey(contents, pemKeyBytes)
	case opensshPrivateKeyPEMHeader, rsaPrivateKeyPEMHeader, genericPrivateKeyPEMHeader:
		return signGitObjectUsingSSHKey(contents, pemKeyBytes)
	}

	return "", ErrUnknownSigningMethod
}

func signGitObjectUsingGPGKey(contents, pemKeyBytes []byte) (string, error) {
	reader := bytes.NewReader(contents)

	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(pemKeyBytes))
	if err != nil {
		return "", err
	}

	sig := new(strings.Builder)
	if err := openpgp.ArmoredDetachSign(sig, keyring[0], reader, nil); err != nil {
		return "", err
	}

	return sig.String(), nil
}

func signGitObjectUsingSSHKey(contents, pemKeyBytes []byte) (string, error) {
	signer, err := ssh.ParsePrivateKey(pemKeyBytes)
	if err != nil {
		return "", err
	}

	sshSig, err := sshsig.Sign(bytes.NewReader(contents), signer, sshsig.HashSHA512, namespaceSSHSignature)
	if err != nil {
		return "", err
	}

	sigBytes := sshsig.Armor(sshSig)

	return string(sigBytes), nil
}

// verifyGitsignSignature handles the Sigstore-specific workflow involved in
// verifying commit or tag signatures issued by gitsign.
func verifyGitsignSignature(ctx context.Context, key *tuf.Key, data, signature []byte) error {
	root, err := fulcioroots.Get()
	if err != nil {
		return errors.Join(ErrVerifyingSigstoreSignature, err)
	}
	intermediate, err := fulcioroots.GetIntermediates()
	if err != nil {
		return errors.Join(ErrVerifyingSigstoreSignature, err)
	}

	verifier, err := gitsignVerifier.NewCertVerifier(
		gitsignVerifier.WithRootPool(root),
		gitsignVerifier.WithIntermediatePool(intermediate),
	)
	if err != nil {
		return errors.Join(ErrVerifyingSigstoreSignature, err)
	}

	verifiedCert, err := verifier.Verify(ctx, data, signature, true)
	if err != nil {
		return ErrIncorrectVerificationKey
	}

	rekor, err := gitsignRekor.NewWithOptions(ctx, signerverifier.RekorServer)
	if err != nil {
		return errors.Join(ErrVerifyingSigstoreSignature, err)
	}

	ctPub, err := cosign.GetCTLogPubs(ctx)
	if err != nil {
		return errors.Join(ErrVerifyingSigstoreSignature, err)
	}

	checkOpts := &cosign.CheckOpts{
		RekorClient:       rekor.Rekor,
		RootCerts:         root,
		IntermediateCerts: intermediate,
		CTLogPubKeys:      ctPub,
		RekorPubKeys:      rekor.PublicKeys(),
		Identities: []cosign.Identity{{
			Issuer:  key.KeyVal.Issuer,
			Subject: key.KeyVal.Identity,
		}},
	}

	if _, err := cosign.ValidateAndUnpackCert(verifiedCert, checkOpts); err != nil {
		return errors.Join(ErrIncorrectVerificationKey, err)
	}

	return nil
}

// verifySSHKeySignature verifies Git signatures issued by SSH keys.
func verifySSHKeySignature(key *tuf.Key, data, signature []byte) error {
	verifier, err := signerverifier.NewSignerVerifierFromTUFKey(key) //nolint:staticcheck
	if err != nil {
		return errors.Join(ErrVerifyingSSHSignature, err)
	}

	publicKey, err := ssh.NewPublicKey(verifier.Public())
	if err != nil {
		return errors.Join(ErrVerifyingSSHSignature, err)
	}

	sshSignature, err := sshsig.Unarmor(signature)
	if err != nil {
		return errors.Join(ErrVerifyingSSHSignature, err)
	}

	if err := sshsig.Verify(bytes.NewReader(data), sshSignature, publicKey, sshSignature.HashAlgorithm, namespaceSSHSignature); err != nil {
		return errors.Join(ErrIncorrectVerificationKey, err)
	}

	return nil
}
