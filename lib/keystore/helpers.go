// Copyright 2019 ChainSafe Systems (ON) Corp.
// This file is part of gossamer.
//
// The gossamer library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The gossamer library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the gossamer library. If not, see <http://www.gnu.org/licenses/>.

package keystore

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChainSafe/gossamer/lib/common"
	"github.com/ChainSafe/gossamer/lib/crypto"
	"github.com/ChainSafe/gossamer/lib/crypto/ed25519"
	"github.com/ChainSafe/gossamer/lib/crypto/secp256k1"
	"github.com/ChainSafe/gossamer/lib/crypto/sr25519"
	"github.com/ChainSafe/gossamer/lib/keyring"
	"github.com/ChainSafe/gossamer/lib/utils"
)

// PrivateKeyToKeypair returns a public, private keypair given a private key
func PrivateKeyToKeypair(priv crypto.PrivateKey) (kp crypto.Keypair, err error) {
	if key, ok := priv.(*sr25519.PrivateKey); ok {
		kp, err = sr25519.NewKeypairFromPrivate(key)
	} else if key, ok := priv.(*ed25519.PrivateKey); ok {
		kp, err = ed25519.NewKeypairFromPrivate(key)
	} else if key, ok := priv.(*secp256k1.PrivateKey); ok {
		kp, err = secp256k1.NewKeypairFromPrivate(key)
	} else {
		return nil, errors.New("cannot decode key: invalid key type")
	}

	return kp, err
}

// DecodePrivateKey turns input bytes into a private key based on the specified key type
func DecodePrivateKey(in []byte, keytype crypto.KeyType) (priv crypto.PrivateKey, err error) {
	if keytype == crypto.Ed25519Type {
		priv, err = ed25519.NewPrivateKey(in)
	} else if keytype == crypto.Sr25519Type {
		priv, err = sr25519.NewPrivateKey(in)
	} else if keytype == crypto.Secp256k1Type {
		priv, err = secp256k1.NewPrivateKey(in)
	} else {
		return nil, errors.New("cannot decode key: invalid key type")
	}

	return priv, err
}

// GenerateKeypair create a new keypair with the corresponding type and saves
// it to datadir/keystore/[public key].key in json format encrypted using the
// specified password and returns the resulting filepath of the new key
func GenerateKeypair(keytype string, datadir string, password []byte) (string, error) {
	if keytype == "" {
		keytype = crypto.Sr25519Type
	}

	var kp crypto.Keypair
	var err error
	if keytype == crypto.Sr25519Type {
		kp, err = sr25519.GenerateKeypair()
		if err != nil {
			return "", fmt.Errorf("failed to generate sr25519 keypair: %s", err)
		}
	} else if keytype == crypto.Ed25519Type {
		kp, err = ed25519.GenerateKeypair()
		if err != nil {
			return "", fmt.Errorf("failed to generate ed25519 keypair: %s", err)
		}
	} else if keytype == crypto.Secp256k1Type {
		kp, err = secp256k1.GenerateKeypair()
		if err != nil {
			return "", fmt.Errorf("failed to generate secp256k1 keypair: %s", err)
		}
	}

	keyPath, err := utils.KeystoreDir(datadir)
	if err != nil {
		return "", fmt.Errorf("failed to get keystore directory: %s", err)
	}

	pub := hex.EncodeToString(kp.Public().Encode())
	fp, err := filepath.Abs(keyPath + "/" + pub + ".key")
	if err != nil {
		return "", fmt.Errorf("failed to create absolute filepath: %s", err)
	}

	file, err := os.OpenFile(fp, os.O_EXCL|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}

	err = EncryptAndWriteToFile(file, kp.Private(), password)
	if err != nil {
		return "", fmt.Errorf("failed to write key to file: %s", err)
	}

	err = file.Close()
	if err != nil {
		return "", fmt.Errorf("failed to close file: %s", err)
	}

	return fp, nil
}

// LoadKeystore loads a new keystore and inserts the test key into the keystore
func LoadKeystore(key string) (*Keystore, error) {
	ks := NewKeystore()

	if key != "" {

		kr, err := keyring.NewKeyring()
		if err != nil {
			return nil, fmt.Errorf("failed to create keyring")
		}

		switch strings.ToLower(key) {
		case "alice":
			ks.Insert(kr.Alice)
		case "bob":
			ks.Insert(kr.Bob)
		case "charlie":
			ks.Insert(kr.Charlie)
		case "dave":
			ks.Insert(kr.Dave)
		case "eve":
			ks.Insert(kr.Eve)
		case "fred":
			ks.Insert(kr.Fred)
		case "george":
			ks.Insert(kr.George)
		case "heather":
			ks.Insert(kr.Heather)
		default:
			return nil, fmt.Errorf("invalid test key provided")
		}
	}

	return ks, nil
}

// ImportKeypair imports a key specified by its filename into a subdirectory
// by the name "keystore" and saves it under the filename "[publickey].key",
// returns the absolute path of the imported key file
func ImportKeypair(fp string, dir string) (string, error) {
	keyDir, err := utils.KeystoreDir(dir)
	if err != nil {
		return "", fmt.Errorf("failed to create keystore directory: %s", err)
	}

	keyData, err := ioutil.ReadFile(filepath.Clean(fp))
	if err != nil {
		return "", fmt.Errorf("failed to read keystore file: %s", err)
	}

	keystore := new(EncryptedKeystore)
	err = json.Unmarshal(keyData, keystore)
	if err != nil {
		return "", fmt.Errorf("failed to read import keystore data: %s", err)
	}

	keyFilePath, err := filepath.Abs(keyDir + "/" + keystore.PublicKey[2:] + ".key")
	if err != nil {
		return "", fmt.Errorf("failed to create keystore filepath: %s", err)
	}

	err = ioutil.WriteFile(keyFilePath, keyData, 0644)
	if err != nil {
		return "", fmt.Errorf("failed to write to keystore file: %s", err)
	}

	return keyFilePath, nil
}

// UnlockKeys unlocks keys specified by the --unlock flag with the passwords given by --password
// and places them into the keystore
func UnlockKeys(ks *Keystore, dir string, unlock string, password string) error {
	var indices []int
	var passwords []string
	var err error

	keyDir, err := utils.KeystoreDir(dir)
	if err != nil {
		return err
	}

	if unlock != "" {
		// indices of keys to unlock
		indices, err = common.StringToInts(unlock)
		if err != nil {
			return err
		}
	}

	if password != "" {
		// passwords corresponding to the keys
		passwords = strings.Split(password, ",")
	}

	if len(passwords) != len(indices) {
		return fmt.Errorf("number of passwords given does not match number of keys to unlock")
	}

	// get paths to key files
	keyFiles, err := utils.KeystoreFiles(dir)
	if err != nil {
		return err
	}

	if len(keyFiles) < len(indices) {
		return fmt.Errorf("number of accounts to unlock is greater than number of accounts in keystore")
	}

	// for each key to unlock, read its file and decrypt contents and add to keystore
	for i, idx := range indices {
		if idx >= len(keyFiles) {
			return fmt.Errorf("invalid account index: %d", idx)
		}

		keyFile := keyFiles[idx]
		priv, err := ReadFromFileAndDecrypt(keyDir+"/"+keyFile, []byte(passwords[i]))
		if err != nil {
			return fmt.Errorf("failed to decrypt key file %s: %s", keyFile, err)
		}

		kp, err := PrivateKeyToKeypair(priv)
		if err != nil {
			return fmt.Errorf("failed to create keypair from private key %d: %s", idx, err)
		}

		ks.Insert(kp)
	}

	return nil
}