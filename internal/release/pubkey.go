package release

import (
	"crypto/ed25519"
	"encoding/hex"
)

// publicKeyHex is the release signing key this build trusts. It is replaced by
// `psgnss-release keygen`, and changing it is a breaking change for every
// station already in the field: a station only accepts releases signed by the
// key its own binary carries, so a new key reaches them by hand or not at all.
//
// An empty value disables updating entirely rather than accepting anything,
// which is what an unreleased development build should do.
const publicKeyHex = "3e9754ba7f44a257ed88cbdd8d2bd2f93271c5290ac982b481e72ed732722676"

// PublicKey returns the release verification key, or nil when this build has
// none. A nil key makes every signature fail to verify, which is the correct
// outcome: a build that cannot tell a real release from a forged one must
// refuse both.
func PublicKey() ed25519.PublicKey {
	raw, err := hex.DecodeString(publicKeyHex)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}

// HaveKey reports whether this build can verify releases at all.
func HaveKey() bool { return PublicKey() != nil }
